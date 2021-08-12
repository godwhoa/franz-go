package kgo

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kmsg"
)

type groupConsumer struct {
	c   *consumer // used to change consumer state; generally c.mu is grabbed on access
	cl  *Client   // used for running requests / adding to topics map
	cfg *cfg

	ctx        context.Context
	cancel     func()
	manageDone chan struct{} // closed once when the manage goroutine quits

	cooperative bool // true if all config balancers are cooperative

	// The data for topics that the user assigned. Metadata updates the
	// atomic.Value in each pointer atomically. If we are consuming via
	// regex, metadata grabs the lock to add new topics.
	tps *topicsPartitions

	reSeen map[string]bool // topics we evaluated against regex, and whether we want them or not

	// Full lock grabbed in CommitOffsetsSync, read lock grabbed in
	// CommitOffsets, this lock ensures that only one sync commit can
	// happen at once, and if it is happening, no other commit can be
	// happening.
	syncCommitMu sync.RWMutex

	rejoinCh chan struct{} // cap 1; sent to if subscription changes (regex)

	// For EOS, before we commit, we force a heartbeat. If the client and
	// group member are both configured properly, then the transactional
	// timeout will be less than the session timeout. By forcing a
	// heartbeat before the commit, if the heartbeat was successful, then
	// we ensure that we will complete the transaction within the group
	// session, meaning we will not commit after the group has rebalanced.
	heartbeatForceCh chan func(error)

	// The following two are only updated in the manager / join&sync loop
	lastAssigned map[string][]int32 // only updated in join&sync loop
	nowAssigned  map[string][]int32 // only updated in join&sync loop

	// leader is whether we are the leader right now. This is set to false
	//
	//  - set to false at the beginning of a join group session
	//  - set to true if join group response indicates we are leader
	//  - read on metadata updates in findNewAssignments
	leader atomicBool

	// Set to true when ending a transaction committing transaction
	// offsets, and then set to false immediately after before calling
	// EndTransaction.
	offsetsAddedToTxn bool

	//////////////
	// mu block //
	//////////////
	mu sync.Mutex

	// using is updated when finding new assignments, we always add to this
	// if we want to consume a topic (or see there are more potential
	// partitions). Only the leader can trigger a new group session if there
	// are simply more partitions for existing topics.
	//
	// This is read when joining a group or leaving a group.
	using map[string]int // topics *we* are currently using => # partitions known in that topic

	// uncommitted is read and updated all over:
	// - updated before PollFetches returns
	// - updated when directly setting offsets (to rewind, for transactions)
	// - emptied when leaving a group
	// - updated when revoking
	// - updated after fetching offsets once we receive our group assignment
	// - updated after we commit
	// - read when getting uncommitted or committed
	uncommitted uncommitted

	// memberID and generation are written to in the join and sync loop,
	// and mostly read within that loop. The reason these two are under the
	// mutex is because they are read during commits, which can happen at
	// any arbitrary moment. It is **recommended** to be done within the
	// context of a group session, but (a) users may have some unique use
	// cases, and (b) the onRevoke hook may take longer than a user
	// expects, which would rotate a session.
	memberID   string
	generation int32

	// commitCancel and commitDone are set under mu before firing off an
	// async commit request. If another commit happens, it cancels the
	// prior commit, waits for the prior to be done, and then starts its
	// own.
	commitCancel func()
	commitDone   chan struct{}

	// blockAuto is set and cleared in CommitOffsets{,Sync} to block
	// autocommitting if autocommitting is active. This ensures that an
	// autocommit does not cancel the user's manual commit.
	blockAuto bool

	dying bool // set when closing, read in findNewAssignments
}

// LeaveGroup leaves a group if in one. Calling the client's Close function
// also leaves a group, so this is only necessary to call if you plan to leave
// the group and continue using the client.
//
// If you have overridden the default revoke, you must manually commit offsets
// before leaving the group.
//
// If you have configured the group with an InstanceID, this does not leave the
// group. With instance IDs, it is expected that clients will restart and
// re-use the same instance ID. To leave a group using an instance ID, you must
// manually issue a kmsg.LeaveGroupRequest or use an external tool (kafka
// scripts or kcl).
func (cl *Client) LeaveGroup() {
	cl.consumer.unset()
}

func (c *consumer) initGroup() {
	ctx, cancel := context.WithCancel(c.cl.ctx)
	g := &groupConsumer{
		c:   c,
		cl:  c.cl,
		cfg: &c.cl.cfg,

		ctx:    ctx,
		cancel: cancel,

		reSeen: make(map[string]bool),

		manageDone:       make(chan struct{}),
		cooperative:      c.cl.cfg.cooperative(),
		tps:              newTopicsPartitions(),
		rejoinCh:         make(chan struct{}, 1),
		heartbeatForceCh: make(chan func(error)),
		using:            make(map[string]int),
	}
	c.g = g
	if !g.cfg.setCommitCallback {
		g.cfg.commitCallback = g.defaultCommitCallback
	}

	if g.cfg.txnID == nil {
		// We only override revoked / lost if they were not explicitly
		// set by options.
		if !g.cfg.setRevoked {
			g.cfg.onRevoked = g.defaultRevoke
		}
		// For onLost, we do not want to commit in onLost, so we
		// explicitly set onLost to an empty function to avoid the
		// fallback to onRevoked.
		if !g.cfg.setLost {
			g.cfg.onLost = func(context.Context, *Client, map[string][]int32) {}
		}
	} else {
		g.cfg.autocommitDisable = true
	}

	// For non-regex topics, we explicitly ensure they exist for loading
	// metadata. This is of no impact if we are *also* consuming via regex,
	// but that is no problem.
	if len(g.cfg.topics) > 0 {
		topics := make([]string, 0, len(g.cfg.topics))
		for topic := range g.cfg.topics {
			topics = append(topics, topic)
		}
		g.tps.storeTopics(topics)
	}

	if !g.cfg.autocommitDisable && g.cfg.autocommitInterval > 0 {
		g.cfg.logger.Log(LogLevelInfo, "beginning autocommit loop", "group", g.cfg.group)
		go g.loopCommit()
	}
}

// Manages the group consumer's join / sync / heartbeat / fetch offset flow.
//
// Once a group is assigned, we fire a metadata request for all topics the
// assignment specified interest in. Only after we finally have some topic
// metadata do we join the group, and once joined, this management runs in a
// dedicated goroutine until the group is left.
func (g *groupConsumer) manage() {
	defer close(g.manageDone)
	g.cfg.logger.Log(LogLevelInfo, "beginning to manage the group lifecycle", "group", g.cfg.group)

	var consecutiveErrors int
	for {
		err := g.joinAndSync()
		if err == nil {
			if err = g.setupAssignedAndHeartbeat(); err != nil {
				if err == kerr.RebalanceInProgress {
					err = nil
				}
			}
		}
		if err == nil {
			consecutiveErrors = 0
			continue
		}

		hook := func() {
			g.cfg.hooks.each(func(h Hook) {
				if h, ok := h.(HookGroupManageError); ok {
					h.OnGroupManageError(err)
				}
			})
		}

		if err == context.Canceled && g.cfg.onRevoked != nil {
			// The cooperative consumer does not revoke everything
			// while rebalancing, meaning if our context is
			// canceled, we may have uncommitted data. Rather than
			// diving into OnLost, we should go into OnRevoked,
			// because for the most part, a context cancelation
			// means we are leaving the group. Going into OnRevoked
			// gives us an opportunity to commit outstanding
			// offsets. For the eager consumer, since we always
			// revoke before exiting the heartbeat loop, we do not
			// really care so much about *needing* to call
			// onRevoked, but since we are handling this case for
			// the cooperative consumer we may as well just also
			// include the eager consumer.
			g.cfg.onRevoked(g.ctx, g.cl, g.nowAssigned)
		} else if g.cfg.onLost != nil {
			// Any other error is perceived as a fatal error,
			// and we go into OnLost as appropriate.
			g.cfg.onLost(g.ctx, g.cl, g.nowAssigned)
			hook()

		} else if g.cfg.onRevoked != nil {
			// If OnLost is not specified, we fallback to OnRevoked.
			g.cfg.onRevoked(g.ctx, g.cl, g.nowAssigned)
			hook()
		}

		// We need to invalidate everything from an error return.
		{
			g.c.mu.Lock()
			g.c.assignPartitions(nil, assignInvalidateAll, nil)
			g.mu.Lock()     // before allowing poll to touch uncommitted, lock the group
			g.c.mu.Unlock() // now part of poll can continue
			g.uncommitted = nil
			g.mu.Unlock()

			g.nowAssigned = nil
			g.lastAssigned = nil

			g.leader.set(false)
		}

		if err == context.Canceled { // context was canceled, quit now
			return
		}

		// Waiting for the backoff is a good time to update our
		// metadata; maybe the error is from stale metadata.
		consecutiveErrors++
		backoff := g.cfg.retryBackoff(consecutiveErrors)
		g.cfg.logger.Log(LogLevelError, "join and sync loop errored",
			"group", g.cfg.group,
			"err", err,
			"consecutive_errors", consecutiveErrors,
			"backoff", backoff,
		)
		deadline := time.Now().Add(backoff)
		g.cl.waitmeta(g.ctx, backoff)
		after := time.NewTimer(time.Until(deadline))
		select {
		case <-g.ctx.Done():
			after.Stop()
			return
		case <-after.C:
		}
	}
}

func (g *groupConsumer) leave() (wait func()) {
	// If g.using is nonzero before this check, then a manage goroutine has
	// started. If not, it will never start because we set dying.
	g.mu.Lock()
	wasDead := g.dying
	g.dying = true
	wasManaging := len(g.using) > 0
	g.mu.Unlock()

	done := make(chan struct{})

	go func() {
		defer close(done)

		g.cancel()

		if wasManaging {
			// We want to wait for the manage goroutine to be done
			// so that we call the user's on{Assign,RevokeLost}.
			<-g.manageDone
		}

		if wasDead {
			// If we already called leave(), then we just wait for
			// the prior leave to finish and we avoid re-issuing a
			// LeaveGroup request.
			return
		}

		if g.cfg.instanceID == nil {
			g.cfg.logger.Log(LogLevelInfo, "leaving group",
				"group", g.cfg.group,
				"member_id", g.memberID, // lock not needed now since nothing can change it (manageDone)
			)
			// If we error when leaving, there is not much
			// we can do. We may as well just return.
			(&kmsg.LeaveGroupRequest{
				Group:    g.cfg.group,
				MemberID: g.memberID,
				Members: []kmsg.LeaveGroupRequestMember{{
					MemberID: g.memberID,
					// no instance ID
				}},
			}).RequestWith(g.cl.ctx, g.cl)
		}
	}()

	return func() { <-done }
}

// returns the difference of g.nowAssigned and g.lastAssigned.
func (g *groupConsumer) diffAssigned() (added, lost map[string][]int32) {
	if g.lastAssigned == nil {
		return g.nowAssigned, nil
	}

	added = make(map[string][]int32, len(g.nowAssigned))
	lost = make(map[string][]int32, len(g.nowAssigned))

	// First, we diff lasts: any topic in last but not now is lost,
	// otherwise, (1) new partitions are added, (2) common partitions are
	// ignored, and (3) partitions no longer in now are lost.
	lasts := make(map[int32]struct{}, 100)
	for topic, lastPartitions := range g.lastAssigned {
		nowPartitions, exists := g.nowAssigned[topic]
		if !exists {
			lost[topic] = lastPartitions
			continue
		}

		for _, lastPartition := range lastPartitions {
			lasts[lastPartition] = struct{}{}
		}

		// Anything now that does not exist in last is new,
		// otherwise it is in common and we ignore it.
		for _, nowPartition := range nowPartitions {
			if _, exists := lasts[nowPartition]; !exists {
				added[topic] = append(added[topic], nowPartition)
			} else {
				delete(lasts, nowPartition)
			}
		}

		// Anything remanining in last does not exist now
		// and is thus lost.
		for last := range lasts {
			lost[topic] = append(lost[topic], last)
			delete(lasts, last) // reuse lasts
		}
	}

	// Finally, any new topics in now assigned are strictly added.
	for topic, nowPartitions := range g.nowAssigned {
		if _, exists := g.lastAssigned[topic]; !exists {
			added[topic] = nowPartitions
		}
	}

	return added, lost
}

type revokeStage int8

const (
	revokeLastSession = iota
	revokeThisSession
)

// revoke calls onRevoked for partitions that this group member is losing and
// updates the uncommitted map after the revoke.
//
// For eager consumers, this simply revokes g.assigned. This will only be
// called at the end of a group session.
//
// For cooperative consumers, this either
//
//     (1) if revoking lost partitions from a prior session (i.e., after sync),
//         this revokes the passed in lost
//     (2) if revoking at the end of a session, this revokes topics that the
//         consumer is no longer interested in consuming (TODO, actually, only
//         once we allow subscriptions to change without leaving the group).
//
// Lastly, for cooperative consumers, this must selectively delete what was
// lost from the uncommitted map.
func (g *groupConsumer) revoke(stage revokeStage, lost map[string][]int32, leaving bool) {
	if !g.cooperative || leaving { // stage == revokeThisSession if not cooperative
		// If we are an eager consumer, we stop fetching all of our
		// current partitions as we will be revoking them.
		g.c.mu.Lock()
		g.c.assignPartitions(nil, assignInvalidateAll, nil)
		g.c.mu.Unlock()

		if !g.cooperative {
			g.cfg.logger.Log(LogLevelInfo, "eager consumer revoking prior assigned partitions", "group", g.cfg.group, "revoking", g.nowAssigned)
		} else {
			g.cfg.logger.Log(LogLevelInfo, "cooperative consumer revoking prior assigned partitions because leaving group", "group", g.cfg.group, "revoking", g.nowAssigned)
		}
		if g.cfg.onRevoked != nil {
			g.cfg.onRevoked(g.ctx, g.cl, g.nowAssigned)
		}
		g.nowAssigned = nil

		// After nilling uncommitted here, nothing should recreate
		// uncommitted until a future fetch after the group is
		// rejoined. This _can_ be broken with a manual SetOffsets or
		// with CommitOffsets{,Sync} but we explicitly document not
		// to do that outside the context of a live group session.
		g.mu.Lock()
		g.uncommitted = nil
		g.mu.Unlock()
		return
	}

	switch stage {
	case revokeLastSession:
		// we use lost in this case

	case revokeThisSession:
		// lost is nil for cooperative assigning. Instead, we determine
		// lost by finding subscriptions we are no longer interested in.
		//
		// TODO only relevant when we allow reassigning with the same
		// group to change subscriptions (also we must delete the
		// unused partitions from nowAssigned).
	}

	if len(lost) > 0 {
		// We must now stop fetching anything we lost and invalidate
		// any buffered fetches before falling into onRevoked.
		//
		// We want to invalidate buffered fetches since they may
		// contain partitions that we lost, and we do not want a future
		// poll to return those fetches.
		lostOffsets := make(map[string]map[int32]Offset, len(lost))

		for lostTopic, lostPartitions := range lost {
			lostPartitionOffsets := make(map[int32]Offset, len(lostPartitions))
			for _, lostPartition := range lostPartitions {
				lostPartitionOffsets[lostPartition] = Offset{}
			}
			lostOffsets[lostTopic] = lostPartitionOffsets
		}

		// We must invalidate before revoking and before updating
		// uncommitted, because we want any commits in onRevoke to be
		// for the final polled offsets. We do not want to allow the
		// logical race of allowing fetches for revoked partitions
		// after a revoke but before an invalidation.
		g.c.mu.Lock()
		g.c.assignPartitions(lostOffsets, assignInvalidateMatching, g.tps)
		g.c.mu.Unlock()
	}

	if len(lost) > 0 || stage == revokeThisSession {
		if len(lost) == 0 {
			g.cfg.logger.Log(LogLevelInfo, "cooperative consumer calling onRevoke at the end of a session even though no partitions were lost", "group", g.cfg.group)
		} else {
			g.cfg.logger.Log(LogLevelInfo, "cooperative consumer calling onRevoke", "group", g.cfg.group, "lost", lost, "stage", stage)
		}
		if g.cfg.onRevoked != nil {
			g.cfg.onRevoked(g.ctx, g.cl, lost)
		}
	}

	if len(lost) == 0 { // if we lost nothing, do nothing
		return
	}

	defer g.rejoin() // cooperative consumers rejoin after they revoking what they lost

	// The block below deletes everything lost from our uncommitted map.
	// All commits should be **completed** by the time this runs. An async
	// commit can undo what we do below. The default revoke runs a sync
	// commit.
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.uncommitted == nil {
		return
	}
	for lostTopic, lostPartitions := range lost {
		uncommittedPartitions := g.uncommitted[lostTopic]
		if uncommittedPartitions == nil {
			continue
		}
		for _, lostPartition := range lostPartitions {
			delete(uncommittedPartitions, lostPartition)
		}
		if len(uncommittedPartitions) == 0 {
			delete(g.uncommitted, lostTopic)
		}
	}
	if len(g.uncommitted) == 0 {
		g.uncommitted = nil
	}
}

// assignRevokeSession aids in sequencing prerevoke/assign/revoke.
type assignRevokeSession struct {
	prerevokeDone chan struct{}
	assignDone    chan struct{}
	revokeDone    chan struct{}
}

func newAssignRevokeSession() *assignRevokeSession {
	return &assignRevokeSession{
		prerevokeDone: make(chan struct{}),
		assignDone:    make(chan struct{}),
		revokeDone:    make(chan struct{}),
	}
}

// For cooperative consumers, the first thing a cooperative consumer does is to
// diff its last assignment and its new assignment and revoke anything lost.
// We call this a "prerevoke".
func (s *assignRevokeSession) prerevoke(g *groupConsumer, lost map[string][]int32) <-chan struct{} {
	go func() {
		defer close(s.prerevokeDone)
		if g.cooperative && len(lost) > 0 {
			g.revoke(revokeLastSession, lost, false)
		}
	}()
	return s.prerevokeDone
}

func (s *assignRevokeSession) assign(g *groupConsumer, newAssigned map[string][]int32) <-chan struct{} {
	go func() {
		defer close(s.assignDone)
		<-s.prerevokeDone
		if g.cfg.onAssigned != nil {
			// We always call on assigned, even if nothing new is
			// assigned. This allows consumers to know that
			// assignment is done and do setup logic.
			g.cfg.onAssigned(g.ctx, g.cl, newAssigned)
		}
	}()
	return s.assignDone
}

// At the end of a group session, before we leave the heartbeat loop, we call
// revoke. For non-cooperative consumers, this revokes everything in the
// current session, and before revoking, we invalidate all partitions.  For the
// cooperative consumer, this does nothing but does notify the client that a
// revoke has begun / the group session is ending.
//
// This may not run before returning from the heartbeat loop: if we encounter a
// fatal error, we return before revoking so that we can instead call onLost in
// the manage loop.
func (s *assignRevokeSession) revoke(g *groupConsumer, leaving bool) <-chan struct{} {
	go func() {
		defer close(s.revokeDone)
		<-s.assignDone
		g.revoke(revokeThisSession, nil, leaving)
	}()
	return s.revokeDone
}

// This chunk of code "pre" revokes lost partitions for the cooperative
// consumer and then begins heartbeating while fetching offsets. This returns
// when heartbeating errors (or if fetch offsets errors).
//
// Before returning, this function ensures that
//  - onAssigned is complete
//    - which ensures that pre revoking is complete
//  - fetching is complete
//  - heartbeating is complete
func (g *groupConsumer) setupAssignedAndHeartbeat() error {
	hbErrCh := make(chan error, 1)
	fetchErrCh := make(chan error, 1)

	s := newAssignRevokeSession()
	added, lost := g.diffAssigned()
	g.cfg.logger.Log(LogLevelInfo, "new group session begun", "group", g.cfg.group, "added", added, "lost", lost)
	s.prerevoke(g, lost) // for cooperative consumers

	// Since we have joined the group, we immediately begin heartbeating.
	// This will continue until the heartbeat errors, the group is killed,
	// or the fetch offsets below errors.
	ctx, cancel := context.WithCancel(g.ctx)
	go func() {
		defer cancel() // potentially kill offset fetching
		g.cfg.logger.Log(LogLevelInfo, "beginning heartbeat loop", "group", g.cfg.group)
		hbErrCh <- g.heartbeat(fetchErrCh, s)
	}()

	// We immediately begin fetching offsets. We want to wait until the
	// fetch function returns, since it assumes within it that another
	// assign cannot happen (it assigns partitions itself). Returning
	// before the fetch completes would be not good.
	//
	// The difference between fetchDone and fetchErrCh is that fetchErrCh
	// can kill heartbeating, or signal it to continue, while fetchDone
	// is specifically used for this function's return.
	fetchDone := make(chan struct{})
	defer func() { <-fetchDone }()
	if len(added) > 0 {
		go func() {
			defer close(fetchDone)
			defer close(fetchErrCh)
			g.cfg.logger.Log(LogLevelInfo, "fetching offsets for added partitions", "group", g.cfg.group, "added", added)
			fetchErrCh <- g.fetchOffsets(ctx, added)
		}()
	} else {
		close(fetchDone)
		close(fetchErrCh)
	}

	// Before we return, we also want to ensure that the user's onAssign is
	// done.
	//
	// Ensuring assigning is done ensures two things:
	//
	// * that we wait for for prerevoking to be done, which updates the
	// uncommitted field. Waiting for that ensures that a rejoin and poll
	// does not have weird concurrent interaction.
	//
	// * that our onLost will not be concurrent with onAssign
	//
	// We especially need to wait here because heartbeating may not
	// necessarily run onRevoke before returning (because of a fatal
	// error).
	s.assign(g, added)
	defer func() { <-s.assignDone }()

	// Finally, we simply return whatever the heartbeat error is. This will
	// be the fetch offset error if that function is what killed this.
	return <-hbErrCh
}

// heartbeat issues heartbeat requests to Kafka for the duration of a group
// session.
//
// This function begins before fetching offsets to allow the consumer's
// onAssigned to be called before fetching. If the eventual offset fetch
// errors, we continue heartbeating until onRevoked finishes and our metadata
// is updated. If the error is not RebalanceInProgress, we return immediately.
//
// If the offset fetch is successful, then we basically sit in this function
// until a heartbeat errors or we, being the leader, decide to re-join.
func (g *groupConsumer) heartbeat(fetchErrCh <-chan error, s *assignRevokeSession) error {
	ticker := time.NewTicker(g.cfg.heartbeatInterval)
	defer ticker.Stop()

	// We issue one heartbeat quickly if we are cooperative because
	// cooperative consumers rejoin the group immediately, and we want to
	// detect that in 500ms rather than 3s.
	var cooperativeFastCheck <-chan time.Time
	if g.cooperative {
		cooperativeFastCheck = time.After(500 * time.Millisecond)
	}

	var metadone, revoked <-chan struct{}
	var heartbeat, didMetadone, didRevoke bool
	var lastErr error

	ctxCh := g.ctx.Done()

	for {
		var err error
		var force func(error)
		heartbeat = false
		select {
		case <-cooperativeFastCheck:
			heartbeat = true
		case <-ticker.C:
			heartbeat = true
		case force = <-g.heartbeatForceCh:
			heartbeat = true
		case <-g.rejoinCh:
			// If a metadata update changes our subscription,
			// we just pretend we are rebalancing.
			err = kerr.RebalanceInProgress
		case err = <-fetchErrCh:
			fetchErrCh = nil
		case <-metadone:
			metadone = nil
			didMetadone = true
		case <-revoked:
			revoked = nil
			didRevoke = true
		case <-ctxCh:
			// Even if the group is left, we need to wait for our
			// revoke to finish before returning, otherwise the
			// manage goroutine will race with us setting
			// nowAssigned.
			ctxCh = nil
			err = context.Canceled
		}

		if heartbeat {
			g.cfg.logger.Log(LogLevelDebug, "heartbeating", "group", g.cfg.group)
			req := &kmsg.HeartbeatRequest{
				Group:      g.cfg.group,
				Generation: g.generation,
				MemberID:   g.memberID,
				InstanceID: g.cfg.instanceID,
			}
			var resp *kmsg.HeartbeatResponse
			if resp, err = req.RequestWith(g.ctx, g.cl); err == nil {
				err = kerr.ErrorForCode(resp.ErrorCode)
			}
			g.cfg.logger.Log(LogLevelDebug, "heartbeat complete", "group", g.cfg.group, "err", err)
			if force != nil {
				force(err)
			}
		}

		// The first error either triggers a clean revoke and metadata
		// update or it returns immediately. If we triggered the
		// revoke, we wait for it to complete regardless of any future
		// error.
		if didMetadone && didRevoke {
			g.cfg.logger.Log(LogLevelInfo, "heartbeat loop complete", "group", g.cfg.group, "err", lastErr)
			return lastErr
		}

		if err == nil {
			continue
		}

		if lastErr == nil {
			g.cfg.logger.Log(LogLevelInfo, "heartbeat errored", "group", g.cfg.group, "err", err)
		} else {
			g.cfg.logger.Log(LogLevelInfo, "heartbeat errored again while waiting for user revoke to finish", "group", g.cfg.group, "err", err)
		}

		// Since we errored, we must revoke.
		if !didRevoke && revoked == nil {
			// If our error is not from rebalancing, then we
			// encountered IllegalGeneration or UnknownMemberID or
			// our context closed all of which are unexpected and
			// unrecoverable.
			//
			// We return early rather than revoking and updating
			// metadata; the groupConsumer's manage function will
			// call onLost with all partitions.
			//
			// setupAssignedAndHeartbeat still waits for onAssigned
			// to be done so that we avoid calling onLost
			// concurrently.
			if err != kerr.RebalanceInProgress && revoked == nil {
				return err
			}

			// Now we call the user provided revoke callback, even
			// if cooperative: if cooperative, this only revokes
			// partitions we no longer want to consume.
			//
			// If the err is context.Canceled, the group is being
			// left and we revoke everything.
			revoked = s.revoke(g, err == context.Canceled)
		}
		// Since we errored, while waiting for the revoke to finish, we
		// update our metadata. A leader may have re-joined with new
		// metadata, and we want the update.
		if !didMetadone && metadone == nil {
			waited := make(chan struct{})
			metadone = waited
			go func() {
				g.cl.waitmeta(g.ctx, g.cfg.sessionTimeout)
				close(waited)
			}()
		}

		// We always save the latest error; generally this should be
		// REBALANCE_IN_PROGRESS, but if the revoke takes too long,
		// Kafka may boot us and we will get a different error.
		lastErr = err
	}
}

// ForceRebalance quits a group member's heartbeat loop so that the member
// rejoins with a JoinGroupRequest.
//
// This function is only useful if you either (a) know that the group member is
// a leader, and want to force a rebalance for any particular reason, or (b)
// are using a custom group balancer, and have changed the metadata that will
// be returned from its JoinGroupMetadata method. This function has no other
// use; see KIP-568 for more details around this function's motivation.
//
// If neither of the cases above are true (this member is not a leader, and the
// join group metadata has not changed), then Kafka will not actually trigger a
// rebalance and will instead reply to the member with its current assignment.
func (cl *Client) ForceRebalance() {
	if g := cl.consumer.g; g != nil {
		g.rejoin()
	}
}

// rejoin is called after a cooperative member revokes what it lost at the
// beginning of a session, or if we are leader and detect new partitions to
// consume.
func (g *groupConsumer) rejoin() {
	select {
	case g.rejoinCh <- struct{}{}:
	default:
	}
}

// Joins and then syncs, issuing the two slow requests in goroutines to allow
// for group cancelation to return early.
func (g *groupConsumer) joinAndSync() error {
	g.cfg.logger.Log(LogLevelInfo, "joining group", "group", g.cfg.group)
	g.leader.set(false)

start:
	select {
	case <-g.rejoinCh: // drain to avoid unnecessary rejoins
	default:
	}

	var (
		joinReq = &kmsg.JoinGroupRequest{
			Group:                  g.cfg.group,
			SessionTimeoutMillis:   int32(g.cfg.sessionTimeout.Milliseconds()),
			RebalanceTimeoutMillis: int32(g.cfg.rebalanceTimeout.Milliseconds()),
			ProtocolType:           g.cfg.protocol,
			MemberID:               g.memberID,
			InstanceID:             g.cfg.instanceID,
			Protocols:              g.joinGroupProtocols(),
		}

		joinResp *kmsg.JoinGroupResponse
		err      error
		joined   = make(chan struct{})
	)

	go func() {
		defer close(joined)
		joinResp, err = joinReq.RequestWith(g.ctx, g.cl)
	}()

	select {
	case <-joined:
	case <-g.ctx.Done():
		return g.ctx.Err() // group killed
	}
	if err != nil {
		return err
	}

	restart, protocol, plan, err := g.handleJoinResp(joinResp)
	if restart {
		goto start
	}
	if err != nil {
		g.cfg.logger.Log(LogLevelWarn, "join group failed", "group", g.cfg.group, "err", err)
		return err
	}

	var (
		syncReq = &kmsg.SyncGroupRequest{
			Group:           g.cfg.group,
			Generation:      g.generation,
			MemberID:        g.memberID,
			InstanceID:      g.cfg.instanceID,
			ProtocolType:    &g.cfg.protocol,
			Protocol:        &protocol,
			GroupAssignment: plan, // nil unless we are the leader
		}

		syncResp *kmsg.SyncGroupResponse
		synced   = make(chan struct{})
	)

	g.cfg.logger.Log(LogLevelInfo, "syncing", "group", g.cfg.group, "protocol_type", g.cfg.protocol, "protocol", protocol)
	go func() {
		defer close(synced)
		syncResp, err = syncReq.RequestWith(g.ctx, g.cl)
	}()

	select {
	case <-synced:
	case <-g.ctx.Done():
		return g.ctx.Err()
	}
	if err != nil {
		return err
	}

	if err = g.handleSyncResp(protocol, syncResp); err != nil {
		if err == kerr.RebalanceInProgress {
			g.cfg.logger.Log(LogLevelInfo, "sync failed with RebalanceInProgress, rejoining", "group", g.cfg.group)
			goto start
		}
		g.cfg.logger.Log(LogLevelWarn, "sync group failed", "group", g.cfg.group, "err", err)
		return err
	}

	return nil
}

func (g *groupConsumer) handleJoinResp(resp *kmsg.JoinGroupResponse) (restart bool, protocol string, plan []kmsg.SyncGroupRequestGroupAssignment, err error) {
	if err = kerr.ErrorForCode(resp.ErrorCode); err != nil {
		switch err {
		case kerr.MemberIDRequired:
			g.mu.Lock()
			g.memberID = resp.MemberID // KIP-394
			g.mu.Unlock()
			g.cfg.logger.Log(LogLevelInfo, "join returned MemberIDRequired, rejoining with response's MemberID", "group", g.cfg.group, "member_id", resp.MemberID)
			return true, "", nil, nil
		case kerr.UnknownMemberID:
			g.mu.Lock()
			g.memberID = ""
			g.mu.Unlock()
			g.cfg.logger.Log(LogLevelInfo, "join returned UnknownMemberID, rejoining without a member id", "group", g.cfg.group)
			return true, "", nil, nil
		}
		return // Request retries as necesary, so this must be a failure
	}

	// Concurrent committing, while erroneous to do at the moment, could
	// race with this function. We need to lock setting these two fields.
	g.mu.Lock()
	g.memberID = resp.MemberID
	g.generation = resp.Generation
	g.mu.Unlock()

	if resp.Protocol != nil {
		protocol = *resp.Protocol
	}

	leader := resp.LeaderID == resp.MemberID
	if leader {
		g.leader.set(true)
		g.cfg.logger.Log(LogLevelInfo, "joined, balancing group",
			"group", g.cfg.group,
			"member_id", g.memberID,
			"instance_id", g.cfg.instanceID,
			"generation", g.generation,
			"balance_protocol", protocol,
			"leader", true,
		)

		plan, err = g.balanceGroup(protocol, resp.Members)
		if err != nil {
			return
		}

	} else {
		g.cfg.logger.Log(LogLevelInfo, "joined",
			"group", g.cfg.group,
			"member_id", g.memberID,
			"instance_id", g.cfg.instanceID,
			"generation", g.generation,
			"leader", false,
		)
	}
	return
}

func (g *groupConsumer) handleSyncResp(protocol string, resp *kmsg.SyncGroupResponse) error {
	if err := kerr.ErrorForCode(resp.ErrorCode); err != nil {
		return err
	}

	b, err := g.findBalancer("sync assignment", protocol)
	if err != nil {
		return err
	}

	assigned, err := b.ParseSyncAssignment(resp.MemberAssignment)
	if err != nil {
		g.cfg.logger.Log(LogLevelError, "sync assignment parse failed", "group", g.cfg.group, "err", err)
		return err
	}

	var sb strings.Builder
	for topic, partitions := range assigned {
		fmt.Fprintf(&sb, "%s%v", topic, partitions)
		sb.WriteString(", ")
	}
	g.cfg.logger.Log(LogLevelInfo, "synced", "group", g.cfg.group, "assigned", strings.TrimSuffix(sb.String(), ", "))

	// Past this point, we will fall into the setupAssigned prerevoke code,
	// meaning for cooperative, we will revoke what we need to.
	if g.cooperative {
		g.lastAssigned = g.nowAssigned
	}
	g.nowAssigned = assigned
	return nil
}

func (g *groupConsumer) joinGroupProtocols() []kmsg.JoinGroupRequestProtocol {
	g.mu.Lock()

	topics := make([]string, 0, len(g.using))
	for topic := range g.using {
		topics = append(topics, topic)
	}
	nowDup := make(map[string][]int32) // deep copy to allow modifications
	for topic, partitions := range g.nowAssigned {
		nowDup[topic] = append([]int32(nil), partitions...)
	}
	gen := g.generation

	g.mu.Unlock()

	sort.Strings(topics) // we guarantee to JoinGroupMetadata that the input strings are sorted
	for _, partitions := range nowDup {
		sort.Slice(partitions, func(i, j int) bool { return partitions[i] < partitions[j] }) // same for partitions
	}

	var protos []kmsg.JoinGroupRequestProtocol
	for _, balancer := range g.cfg.balancers {
		protos = append(protos, kmsg.JoinGroupRequestProtocol{
			Name:     balancer.ProtocolName(),
			Metadata: balancer.JoinGroupMetadata(topics, nowDup, gen),
		})
	}
	return protos
}

// fetchOffsets is issued once we join a group to see what the prior commits
// were for the partitions we were assigned.
func (g *groupConsumer) fetchOffsets(ctx context.Context, newAssigned map[string][]int32) error {
	// Our client maps the v0 to v7 format to v8+ when sharding this
	// request, if we are only requesting one group, as well as maps the
	// response back, so we do not need to worry about v8+ here.
start:
	req := kmsg.OffsetFetchRequest{
		Group:         g.cfg.group,
		RequireStable: g.cfg.requireStable,
	}
	for topic, partitions := range newAssigned {
		req.Topics = append(req.Topics, kmsg.OffsetFetchRequestTopic{
			Topic:      topic,
			Partitions: partitions,
		})
	}

	var (
		resp *kmsg.OffsetFetchResponse
		err  error
	)

	fetchDone := make(chan struct{})
	go func() {
		defer close(fetchDone)
		resp, err = req.RequestWith(ctx, g.cl)
	}()
	select {
	case <-fetchDone:
	case <-ctx.Done():
		g.cfg.logger.Log(LogLevelError, "fetch offsets failed due to context cancelation", "group", g.cfg.group)
		return ctx.Err()
	}
	if err != nil {
		g.cfg.logger.Log(LogLevelError, "fetch offsets failed with non-retriable error", "group", g.cfg.group, "err", err)
		return err
	}

	// Even if a leader epoch is returned, if brokers do not support
	// OffsetForLeaderEpoch for some reason (odd set of supported reqs), we
	// cannot use the returned leader epoch.
	kip320 := g.cl.supportsOffsetForLeaderEpoch()

	offsets := make(map[string]map[int32]Offset)
	for _, rTopic := range resp.Topics {
		topicOffsets := make(map[int32]Offset)
		offsets[rTopic.Topic] = topicOffsets
		for _, rPartition := range rTopic.Partitions {
			if err = kerr.ErrorForCode(rPartition.ErrorCode); err != nil {
				// KIP-447: Unstable offset commit means there is a
				// pending transaction that should be committing soon.
				// We sleep for 1s and retry fetching offsets.
				if err == kerr.UnstableOffsetCommit {
					g.cfg.logger.Log(LogLevelInfo, "fetch offsets failed with UnstableOffsetCommit, waiting 1s and retrying",
						"group", g.cfg.group,
						"topic", rTopic.Topic,
						"partition", rPartition.Partition,
					)
					select {
					case <-ctx.Done():
					case <-time.After(time.Second):
						goto start
					}
				}
				return err
			}
			offset := Offset{
				at:    rPartition.Offset,
				epoch: -1,
			}
			if resp.Version >= 5 && kip320 { // KIP-320
				offset.epoch = rPartition.LeaderEpoch
			}
			if rPartition.Offset == -1 {
				offset = g.cfg.resetOffset
			}
			topicOffsets[rPartition.Partition] = offset
		}
	}

	groupTopics := g.tps.load()
	for fetchedTopic := range offsets {
		if !groupTopics.hasTopic(fetchedTopic) {
			delete(offsets, fetchedTopic)
			g.cfg.logger.Log(LogLevelWarn, "member was assigned topic that we did not ask for in ConsumeTopics! skipping assigning this topic!", "group", g.cfg.group, "topic", fetchedTopic)
		}
	}

	// Lock for assign and then updating uncommitted.
	g.c.mu.Lock()
	defer g.c.mu.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()

	// Eager: we already invalidated everything; nothing to re-invalidate.
	// Cooperative: assign without invalidating what we are consuming.
	g.c.assignPartitions(offsets, assignWithoutInvalidating, g.tps)

	// We need to update the uncommited map so that SetOffsets(Committed)
	// does not rewind before the committed offsets we just fetched.
	if g.uncommitted == nil {
		g.uncommitted = make(uncommitted, 10)
	}
	for topic, partitions := range offsets {
		topicUncommitted := g.uncommitted[topic]
		if topicUncommitted == nil {
			topicUncommitted = make(map[int32]uncommit, 20)
			g.uncommitted[topic] = topicUncommitted
		}
		for partition, offset := range partitions {
			if offset.at < 0 {
				continue // not yet committed
			}
			committed := EpochOffset{
				Epoch:  offset.epoch,
				Offset: offset.at,
			}
			topicUncommitted[partition] = uncommit{
				head:      committed,
				committed: committed,
			}
		}
	}

	if g.cfg.logger.Level() >= LogLevelDebug {
		g.cfg.logger.Log(LogLevelDebug, "fetched committed offsets", "group", g.cfg.group, "fetched", offsets)
	} else {
		g.cfg.logger.Log(LogLevelInfo, "fetched committed offsets", "group", g.cfg.group)
	}
	return nil
}

// findNewAssignments updates topics the group wants to use and other metadata.
// We only grab the group mu at the end if we need to.
//
// This joins the group if
//  - the group has never been joined
//  - new topics are found for consuming (changing this consumer's join metadata)
//
// Additionally, if the member is the leader, this rejoins the group if the
// leader notices new partitions in an existing topic.
//
// This does not rejoin if the leader notices a partition is lost, which is
// finicky.
func (g *groupConsumer) findNewAssignments() {
	topics := g.tps.load()

	type change struct {
		isNew bool
		delta int
	}

	var numNewTopics int
	toChange := make(map[string]change, len(topics))
	for topic, topicPartitions := range topics {
		parts := topicPartitions.load()
		numPartitions := len(parts.partitions)
		// If we are already using this topic, add that it changed if
		// there are more partitions than we were using prior.
		if used, exists := g.using[topic]; exists {
			if added := numPartitions - used; added > 0 {
				toChange[topic] = change{delta: added}
			}
			continue
		}

		var useTopic bool
		if g.cfg.regex {
			want, seen := g.reSeen[topic]
			if !seen {
				for _, re := range g.cfg.topics {
					if want = re.MatchString(topic); want {
						break
					}
				}
				g.reSeen[topic] = want
			}
			useTopic = want
		} else {
			_, useTopic = g.cfg.topics[topic]
		}

		// We only track using the topic if there are partitions for
		// it; if there are none, then the topic was set by _us_ as "we
		// want to load the metadata", but the topic was not returned
		// in the metadata (or it was returned with an error).
		if useTopic && numPartitions > 0 {
			if g.cfg.regex && parts.isInternal {
				continue
			}
			toChange[topic] = change{isNew: true, delta: numPartitions}
			numNewTopics++
		}

	}

	if len(toChange) == 0 {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.dying {
		return
	}

	wasManaging := len(g.using) != 0
	for topic, change := range toChange {
		g.using[topic] += change.delta
	}

	if !wasManaging {
		go g.manage()
		return
	}

	if numNewTopics > 0 || g.leader.get() {
		g.rejoin()
	}
}

// uncommit tracks the latest offset polled (+1) and the latest commit.
// The reason head is just past the latest offset is because we want
// to commit TO an offset, not BEFORE an offset.
type uncommit struct {
	head      EpochOffset
	committed EpochOffset
}

// EpochOffset combines a record offset with the leader epoch the broker
// was at when the record was written.
type EpochOffset struct {
	Epoch  int32
	Offset int64
}

type uncommitted map[string]map[int32]uncommit

// updateUncommitted sets the latest uncommitted offset.
func (g *groupConsumer) updateUncommitted(fetches Fetches) {
	var b bytes.Buffer
	debug := g.cfg.logger.Level() >= LogLevelDebug

	g.mu.Lock()
	defer g.mu.Unlock()

	for _, fetch := range fetches {
		for _, topic := range fetch.Topics {

			if debug {
				fmt.Fprintf(&b, "%s[", topic.Topic)
			}

			var topicOffsets map[int32]uncommit
			for _, partition := range topic.Partitions {
				if len(partition.Records) == 0 {
					continue
				}
				final := partition.Records[len(partition.Records)-1]

				if topicOffsets == nil {
					if g.uncommitted == nil {
						g.uncommitted = make(uncommitted, 10)
					}
					topicOffsets = g.uncommitted[topic.Topic]
					if topicOffsets == nil {
						topicOffsets = make(map[int32]uncommit, 20)
						g.uncommitted[topic.Topic] = topicOffsets
					}
				}

				uncommit := topicOffsets[partition.Partition]

				// Our new head points just past the final consumed offset,
				// that is, if we rejoin, this is the offset to begin at.
				newOffset := final.Offset + 1
				if debug {
					fmt.Fprintf(&b, "%d{%d=>%d}, ", partition.Partition, uncommit.head.Offset, newOffset)
				}
				uncommit.head = EpochOffset{
					final.LeaderEpoch, // -1 if old message / unknown
					newOffset,
				}
				topicOffsets[partition.Partition] = uncommit
			}

			if debug {
				if bytes.HasSuffix(b.Bytes(), []byte(", ")) {
					b.Truncate(b.Len() - 2)
				}
				b.WriteString("], ")
			}
		}
	}

	if debug {
		update := b.String()
		update = strings.TrimSuffix(update, ", ") // trim trailing comma and space after final topic
		g.cfg.logger.Log(LogLevelDebug, "updated uncommitted", "group", g.cfg.group, "to", update)
	}
}

// updateCommitted updates the group's uncommitted map. This function triply
// verifies that the resp matches the req as it should and that the req does
// not somehow contain more than what is in our uncommitted map.
func (g *groupConsumer) updateCommitted(
	req *kmsg.OffsetCommitRequest,
	resp *kmsg.OffsetCommitResponse,
) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if req.Generation != g.generation {
		return
	}
	if g.uncommitted == nil || // just in case
		len(req.Topics) != len(resp.Topics) { // bad kafka
		g.cfg.logger.Log(LogLevelError, fmt.Sprintf("Kafka replied to our OffsetCommitRequest incorrectly! Num topics in request: %d, in reply: %d, we cannot handle this!", len(req.Topics), len(resp.Topics)), "group", g.cfg.group)
		return
	}

	sort.Slice(req.Topics, func(i, j int) bool {
		return req.Topics[i].Topic < req.Topics[j].Topic
	})
	sort.Slice(resp.Topics, func(i, j int) bool {
		return resp.Topics[i].Topic < resp.Topics[j].Topic
	})

	var b bytes.Buffer
	debug := g.cfg.logger.Level() >= LogLevelDebug

	for i := range resp.Topics {
		reqTopic := &req.Topics[i]
		respTopic := &resp.Topics[i]
		topic := g.uncommitted[respTopic.Topic]
		if topic == nil || // just in case
			reqTopic.Topic != respTopic.Topic || // bad kafka
			len(reqTopic.Partitions) != len(respTopic.Partitions) { // same
			g.cfg.logger.Log(LogLevelError, fmt.Sprintf("Kafka replied to our OffsetCommitRequest incorrectly! Topic at request index %d: %s, reply at index: %s; num partitions on request topic: %d, in reply: %d, we cannot handle this!", i, reqTopic.Topic, respTopic.Topic, len(reqTopic.Partitions), len(respTopic.Partitions)), "group", g.cfg.group)
			continue
		}

		sort.Slice(reqTopic.Partitions, func(i, j int) bool {
			return reqTopic.Partitions[i].Partition < reqTopic.Partitions[j].Partition
		})
		sort.Slice(respTopic.Partitions, func(i, j int) bool {
			return respTopic.Partitions[i].Partition < respTopic.Partitions[j].Partition
		})

		if debug {
			fmt.Fprintf(&b, "%s[", respTopic.Topic)
		}
		for i := range respTopic.Partitions {
			reqPart := &reqTopic.Partitions[i]
			respPart := &respTopic.Partitions[i]
			uncommit, exists := topic[respPart.Partition]
			if !exists { // just in case
				continue
			}
			if reqPart.Partition != respPart.Partition { // bad kafka
				g.cfg.logger.Log(LogLevelError, fmt.Sprintf("Kafka replied to our OffsetCommitRequest incorrectly! Topic %s partition %d != resp partition %d", reqTopic.Topic, reqPart.Partition, respPart.Partition), "group", g.cfg.group)
				continue
			}
			if respPart.ErrorCode != 0 {
				g.cfg.logger.Log(LogLevelWarn, "unable to commit offset for topic partition", "group", g.cfg.group, "topic", reqTopic.Topic, "partition", reqPart.Partition, "error_code", respPart.ErrorCode)
				continue
			}

			if debug {
				fmt.Fprintf(&b, "%d{%d=>%d}, ", reqPart.Partition, uncommit.committed.Offset, reqPart.Offset)
			}

			uncommit.committed = EpochOffset{
				reqPart.LeaderEpoch,
				reqPart.Offset,
			}
			topic[respPart.Partition] = uncommit
		}

		if debug {
			if bytes.HasSuffix(b.Bytes(), []byte(", ")) {
				b.Truncate(b.Len() - 2)
			}
			b.WriteString("], ")
		}

	}

	if debug {
		update := b.String()
		update = strings.TrimSuffix(update, ", ") // trim trailing comma and space after final topic
		g.cfg.logger.Log(LogLevelDebug, "updated committed", "group", g.cfg.group, "to", update)
	}
}

func (g *groupConsumer) defaultCommitCallback(_ *Client, _ *kmsg.OffsetCommitRequest, resp *kmsg.OffsetCommitResponse, err error) {
	if err != nil {
		if err != context.Canceled {
			g.cfg.logger.Log(LogLevelError, "default commit failed", "group", g.cfg.group, "err", err)
		} else {
			g.cfg.logger.Log(LogLevelDebug, "default commit canceled", "group", g.cfg.group)
		}
		return
	}
	for _, topic := range resp.Topics {
		for _, partition := range topic.Partitions {
			if err := kerr.ErrorForCode(partition.ErrorCode); err != nil {
				g.cfg.logger.Log(LogLevelError, "in default commit: unable to commit offsets for topic partition",
					"group", g.cfg.group,
					"topic", topic.Topic,
					"partition", partition.Partition,
					"error", err)
			}
		}
	}
}

func (g *groupConsumer) loopCommit() {
	ticker := time.NewTicker(g.cfg.autocommitInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-g.ctx.Done():
			return
		}

		// We use the group context for the default autocommit; revokes
		// use the client context so that we can be sure we commit even
		// after the group context is canceled (which is the first
		// thing that happens so as to quit the manage loop before
		// leaving a group).
		g.mu.Lock()
		if !g.blockAuto {
			g.cfg.logger.Log(LogLevelDebug, "autocommitting", "group", g.cfg.group)
			g.commit(g.ctx, g.getUncommittedLocked(true), g.cfg.commitCallback)
		}
		g.mu.Unlock()
	}
}

// SetOffsets, for consumer groups, sets any matching offsets in setOffsets to
// the given epoch/offset. Partitions that are not specified are not set. It is
// invalid to set topics that were not yet returned from a PollFetches.
//
// If using transactions, it is advised to just use a GroupTransactSession and
// avoid this function entirely.
//
// It is strongly recommended to use this function outside of the context of a
// PollFetches loop and only when you know the group is not revoked (i.e.,
// block any concurrent revoke while issuing this call). Any other usage is
// prone to odd interactions.
func (cl *Client) SetOffsets(setOffsets map[string]map[int32]EpochOffset) {
	if len(setOffsets) == 0 {
		return
	}

	// We assignPartitions before returning, so we grab the consumer lock
	// first to preserve consumer mu => group mu ordering.
	c := &cl.consumer
	c.mu.Lock()
	defer c.mu.Unlock()

	g := c.g
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	groupTopics := g.tps.load()

	// The gist of what follows:
	//
	// We need to set uncommitted.committed; that is the guarantee of this
	// function. However, if, for everything we are setting, the head
	// equals the commit, then we do not need to actually invalidate our
	// current assignments. This is a great optimization for transactions
	// that are resetting their state on abort.
	var assigns map[string]map[int32]Offset
	if g.uncommitted == nil {
		g.uncommitted = make(uncommitted)
	}
	for topic, partitions := range setOffsets {
		if !groupTopics.hasTopic(topic) {
			continue // trying to set a topic that was not assigned...
		}
		topicUncommitted := g.uncommitted[topic]
		if topicUncommitted == nil {
			topicUncommitted = make(map[int32]uncommit)
			g.uncommitted[topic] = topicUncommitted
		}
		var topicAssigns map[int32]Offset
		for partition, epochOffset := range partitions {
			current, exists := topicUncommitted[partition]
			if exists && current.head == epochOffset {
				current.committed = epochOffset
				topicUncommitted[partition] = current
				continue
			}
			if topicAssigns == nil {
				topicAssigns = make(map[int32]Offset, len(partitions))
			}
			topicAssigns[partition] = Offset{
				at:    epochOffset.Offset,
				epoch: epochOffset.Epoch,
			}
			topicUncommitted[partition] = uncommit{
				head:      epochOffset,
				committed: epochOffset,
			}
		}
		if len(topicAssigns) > 0 {
			if assigns == nil {
				assigns = make(map[string]map[int32]Offset, 10)
			}
			assigns[topic] = topicAssigns
		}
	}

	if len(assigns) == 0 {
		return
	}

	c.assignPartitions(assigns, assignSetMatching, g.tps)
}

// UncommittedOffsets returns the latest uncommitted offsets. Uncommitted
// offsets are always updated on calls to PollFetches.
//
// If there are no uncommitted offsets, this returns nil.
//
// Note that, if manually committing, you should be careful with committing
// during group rebalances. You must ensure you commit before the group's
// session timeout is reached, otherwise this client will be kicked from the
// group and the commit will fail.
//
// If using a cooperative balancer, commits while consuming during rebalancing
// may fail with REBALANCE_IN_PROGRESS.
func (cl *Client) UncommittedOffsets() map[string]map[int32]EpochOffset {
	if g := cl.consumer.g; g != nil {
		return g.getUncommitted()
	}
	return nil
}

// CommittedOffsets returns the latest committed offsets. Committed offsets are
// updated from commits or from joining a group and fetching offsets.
//
// If there are no committed offsets, this returns nil.
func (cl *Client) CommittedOffsets() map[string]map[int32]EpochOffset {
	g := cl.consumer.g
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.getUncommittedLocked(false)
}

func (g *groupConsumer) getUncommitted() map[string]map[int32]EpochOffset {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.getUncommittedLocked(true)
}

func (g *groupConsumer) getUncommittedLocked(head bool) map[string]map[int32]EpochOffset {
	if g.uncommitted == nil {
		return nil
	}

	var uncommitted map[string]map[int32]EpochOffset
	for topic, partitions := range g.uncommitted {
		var topicUncommitted map[int32]EpochOffset
		for partition, uncommit := range partitions {
			if head && uncommit.head == uncommit.committed {
				continue
			}
			if topicUncommitted == nil {
				if uncommitted == nil {
					uncommitted = make(map[string]map[int32]EpochOffset, len(g.uncommitted))
				}
				topicUncommitted = uncommitted[topic]
				if topicUncommitted == nil {
					topicUncommitted = make(map[int32]EpochOffset, len(partitions))
					uncommitted[topic] = topicUncommitted
				}
			}
			if head {
				topicUncommitted[partition] = uncommit.head
			} else {
				topicUncommitted[partition] = uncommit.committed
			}
		}
	}
	return uncommitted
}

// CommitRecords issues a synchronous offset commit for the offsets contained
// within rs. Retriable errors are retried up to the configured retry limit,
// and any unretriable error is returned.
//
// This function is useful as a simple way to commit offsets if you have
// disabled autocommitting. As an alternative if you always want to commit
// everything, see CommitUncommittedOffsets.
//
// Simple usage of this function may lead to duplicate records if a consumer
// group rebalance occurs before or while this function is being executed. You
// can avoid this scenario by calling CommitRecords in a custom OnRevoked, but
// for most workloads, a small bit of potential duplicate processing is fine.
// See the documentation on DisableAutoCommit for more details.
//
// It is recommended to always commit records in order (per partition). If you
// call this function twice with record for partition 0 at offset 999
// initially, and then with record for partition 0 at offset 4, you will rewind
// your commit.
//
// A use case for this function may be to partially process a batch of records,
// commit, and then continue to process the rest of the records. It is not
// recommended to call this for every record processed in a high throughput
// scenario, because you do not want to unnecessarily increase load on Kafka.
//
// If you do not want to wait for this function to complete before continuing
// processing records, you can call this function in a goroutine.
func (cl *Client) CommitRecords(ctx context.Context, rs ...*Record) error {
	// First build the offset commit map. We favor the latest epoch, then
	// offset, if any records map to the same topic / partition.
	offsets := make(map[string]map[int32]EpochOffset)
	for _, r := range rs {
		toffsets := offsets[r.Topic]
		if toffsets == nil {
			toffsets = make(map[int32]EpochOffset)
			offsets[r.Topic] = toffsets
		}

		if at, exists := toffsets[r.Partition]; exists {
			if at.Epoch > r.LeaderEpoch || at.Epoch == r.LeaderEpoch && at.Offset > r.Offset {
				continue
			}
		}
		toffsets[r.Partition] = EpochOffset{
			r.LeaderEpoch,
			r.Offset,
		}
	}

	var rerr error // return error

	// Our client retries an OffsetCommitRequest as necessary if the first
	// response partition has a retriable group error (group coordinator
	// loading, etc), so any partition error is fatal.
	cl.CommitOffsetsSync(ctx, offsets, func(_ *Client, _ *kmsg.OffsetCommitRequest, resp *kmsg.OffsetCommitResponse, err error) {
		if err != nil {
			rerr = err
			return
		}

		for _, topic := range resp.Topics {
			for _, partition := range topic.Partitions {
				if err := kerr.ErrorForCode(partition.ErrorCode); err != nil {
					rerr = err
					return
				}
			}
		}
	})

	return rerr
}

// CommitUncommittedOffsets issues a synchronous offset commit for any
// partition that has been consumed from that has uncommitted offsets.
// Retriable errors are retried up to the configured retry limit, and any
// unretriable error is returned.
//
// This function is useful as a simple way to commit offsets if you have
// disabled autocommitting. As an alternative if you want to commit specific
// records, see CommitRecords.
//
// Simple usage of this function may lead to duplicate records if a consumer
// group rebalance occurs before or while this function is being executed. You
// can avoid this scenario by calling CommitRecords in a custom OnRevoked, but
// for most workloads, a small bit of potential duplicate processing is fine.
// See the documentation on DisableAutoCommit for more details.
//
// The recommended pattern for using this function is to have a poll / process
// / commit loop. First PollFetches, then process every record, then call
// CommitUncommittedOffsets.
//
// If you do not want to wait for this function to complete before continuing
// processing records, you can call this function in a goroutine.
func (cl *Client) CommitUncommittedOffsets(ctx context.Context) error {
	// This function is just the tail end of CommitRecords just above.
	var rerr error
	cl.CommitOffsetsSync(ctx, cl.UncommittedOffsets(), func(_ *Client, _ *kmsg.OffsetCommitRequest, resp *kmsg.OffsetCommitResponse, err error) {
		if err != nil {
			rerr = err
			return
		}

		for _, topic := range resp.Topics {
			for _, partition := range topic.Partitions {
				if err := kerr.ErrorForCode(partition.ErrorCode); err != nil {
					rerr = err
					return
				}
			}
		}
	})
	return rerr
}

// CommitOffsetsSync cancels any active CommitOffsets, begins a commit that
// cannot be canceled, and waits for that commit to complete. This function
// will not return until the commit is done and the onDone callback is
// complete.
//
// The purpose of this function is for use in OnRevoke or committing before
// leaving a group, because you do not want to have a commit issued in
// OnRevoked canceled.
//
// This is an advanced function, and for simpler, more easily understandable
// committing, see CommitRecords and CommitUncommittedOffsets.
//
// For more information about committing and committing asynchronously, see
// CommitOffsets.
func (cl *Client) CommitOffsetsSync(
	ctx context.Context,
	uncommitted map[string]map[int32]EpochOffset,
	onDone func(*Client, *kmsg.OffsetCommitRequest, *kmsg.OffsetCommitResponse, error),
) {
	if onDone == nil {
		onDone = func(*Client, *kmsg.OffsetCommitRequest, *kmsg.OffsetCommitResponse, error) {}
	}

	g := cl.consumer.g
	if g == nil {
		onDone(cl, new(kmsg.OffsetCommitRequest), new(kmsg.OffsetCommitResponse), errNotGroup)
		return
	}
	if len(uncommitted) == 0 {
		onDone(cl, new(kmsg.OffsetCommitRequest), new(kmsg.OffsetCommitResponse), nil)
		return
	}
	g.commitOffsetsSync(ctx, uncommitted, onDone)
}

func (g *groupConsumer) commitOffsetsSync(
	ctx context.Context,
	uncommitted map[string]map[int32]EpochOffset,
	onDone func(*Client, *kmsg.OffsetCommitRequest, *kmsg.OffsetCommitResponse, error),
) {
	done := make(chan struct{})
	defer func() { <-done }()

	g.cfg.logger.Log(LogLevelDebug, "in CommitOffsetsSync", "group", g.cfg.group, "with", uncommitted)
	defer g.cfg.logger.Log(LogLevelDebug, "left CommitOffsetsSync", "group", g.cfg.group)

	if onDone == nil {
		onDone = func(*Client, *kmsg.OffsetCommitRequest, *kmsg.OffsetCommitResponse, error) {}
	}

	g.syncCommitMu.Lock() // block all other concurrent commits until our OnDone is done.

	unblockCommits := func(cl *Client, req *kmsg.OffsetCommitRequest, resp *kmsg.OffsetCommitResponse, err error) {
		defer close(done)
		defer g.syncCommitMu.Unlock()
		onDone(cl, req, resp, err)
	}

	g.mu.Lock()
	go func() {
		defer g.mu.Unlock()

		g.blockAuto = true
		unblockAuto := func(cl *Client, req *kmsg.OffsetCommitRequest, resp *kmsg.OffsetCommitResponse, err error) {
			unblockCommits(cl, req, resp, err)
			g.mu.Lock()
			defer g.mu.Unlock()
			g.blockAuto = false
		}

		g.commit(ctx, uncommitted, unblockAuto)
	}()
}

// CommitOffsets commits the given offsets for a group, calling onDone with the
// commit request and either the response or an error if the response was not
// issued. If uncommitted is empty or the client is not consuming as a group,
// onDone is called with (nil, nil, nil) and this function returns immediately.
// It is OK if onDone is nil, but you will not know if your commit succeeded.
//
// This is an advanced function and is difficult to use correctly. For simpler,
// more easily understandable committing, see CommitRecords and
// CommitUncommittedOffsets.
//
// This function itself does not wait for the commit to finish. By default,
// this function is an asynchronous commit. You can use onDone to make it sync.
// If autocommitting is enabled, this function blocks autocommitting until this
// function is complete and the onDone has returned.
//
// It is invalid to use this function to commit offsets for a transaction.
//
// Note that this function ensures absolute ordering of commit requests by
// canceling prior requests and ensuring they are done before executing a new
// one. This means, for absolute control, you can use this function to
// periodically commit async and then issue a final sync commit before quitting
// (this is the behavior of autocommiting and using the default revoke). This
// differs from the Java async commit, which does not retry requests to avoid
// trampling on future commits.
//
// It is highly recommended to check the response's partition's error codes if
// the response is non-nil. While unlikely, individual partitions can error.
// This is most likely to happen if a commit occurs too late in a rebalance
// event.
//
// Do not use this async CommitOffsets in OnRevoked, instead use
// CommitOffsetsSync. If you commit async, the rebalance will proceed before
// this function executes, and you will commit offsets for partitions that have
// moved to a different consumer.
func (cl *Client) CommitOffsets(
	ctx context.Context,
	uncommitted map[string]map[int32]EpochOffset,
	onDone func(*Client, *kmsg.OffsetCommitRequest, *kmsg.OffsetCommitResponse, error),
) {
	cl.cfg.logger.Log(LogLevelDebug, "in CommitOffsets", "with", uncommitted)
	defer cl.cfg.logger.Log(LogLevelDebug, "left CommitOffsets")
	if onDone == nil {
		onDone = func(*Client, *kmsg.OffsetCommitRequest, *kmsg.OffsetCommitResponse, error) {}
	}

	g := cl.consumer.g
	if g == nil {
		onDone(cl, new(kmsg.OffsetCommitRequest), new(kmsg.OffsetCommitResponse), errNotGroup)
		return
	}
	if len(uncommitted) == 0 {
		onDone(cl, new(kmsg.OffsetCommitRequest), new(kmsg.OffsetCommitResponse), nil)
		return
	}

	g.syncCommitMu.RLock() // block sync commit, but allow other concurrent Commit to cancel us

	unblockSyncCommit := func(cl *Client, req *kmsg.OffsetCommitRequest, resp *kmsg.OffsetCommitResponse, err error) {
		defer g.syncCommitMu.RUnlock()
		onDone(cl, req, resp, err)
	}

	g.mu.Lock()
	go func() {
		defer g.mu.Unlock()

		g.blockAuto = true
		unblockAuto := func(cl *Client, req *kmsg.OffsetCommitRequest, resp *kmsg.OffsetCommitResponse, err error) {
			unblockSyncCommit(cl, req, resp, err)
			g.mu.Lock()
			defer g.mu.Unlock()
			g.blockAuto = false
		}

		g.commit(ctx, uncommitted, unblockAuto)
	}()
}

// defaultRevoke commits the last fetched offsets and waits for the commit to
// finish. This is the default onRevoked function which, when combined with the
// default autocommit, ensures we never miss committing everything.
//
// Note that the heartbeat loop invalidates all buffered, unpolled fetches
// before revoking, meaning this truly will commit all polled fetches.
func (g *groupConsumer) defaultRevoke(context.Context, *Client, map[string][]int32) {
	if !g.cfg.autocommitDisable {
		// We use the client's context rather than the group context,
		// because this could come from the group being left. The group
		// context will already be canceled.
		g.commitOffsetsSync(g.cl.ctx, g.getUncommitted(), g.cfg.commitCallback)
	}
}

// commit is the logic for Commit; see Commit's documentation
//
// This is called under the groupConsumer's lock.
func (g *groupConsumer) commit(
	ctx context.Context,
	uncommitted map[string]map[int32]EpochOffset,
	onDone func(*Client, *kmsg.OffsetCommitRequest, *kmsg.OffsetCommitResponse, error),
) {
	if onDone == nil { // note we must always call onDone
		onDone = func(*Client, *kmsg.OffsetCommitRequest, *kmsg.OffsetCommitResponse, error) {}
	}
	if len(uncommitted) == 0 { // only empty if called thru autocommit / default revoke
		// We have to do this concurrently because the expectation is
		// that commit itself does not block.
		go onDone(g.cl, new(kmsg.OffsetCommitRequest), new(kmsg.OffsetCommitResponse), nil)
		return
	}

	priorCancel := g.commitCancel
	priorDone := g.commitDone

	commitCtx, commitCancel := context.WithCancel(ctx) // enable ours to be canceled and waited for
	commitDone := make(chan struct{})

	g.commitCancel = commitCancel
	g.commitDone = commitDone

	req := &kmsg.OffsetCommitRequest{
		Group:      g.cfg.group,
		Generation: g.generation,
		MemberID:   g.memberID,
		InstanceID: g.cfg.instanceID,
	}

	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				commitCancel()
			case <-commitCtx.Done():
			}
		}()
	}

	go func() {
		defer close(commitDone) // allow future commits to continue when we are done
		defer commitCancel()
		if priorDone != nil { // wait for any prior request to finish
			select {
			case <-priorDone:
			default:
				g.cfg.logger.Log(LogLevelDebug, "canceling prior commit to issue another", "group", g.cfg.group)
				priorCancel()
				<-priorDone
			}
		}
		g.cfg.logger.Log(LogLevelDebug, "issuing commit", "group", g.cfg.group, "uncommitted", uncommitted)

		for topic, partitions := range uncommitted {
			req.Topics = append(req.Topics, kmsg.OffsetCommitRequestTopic{
				Topic: topic,
			})
			reqTopic := &req.Topics[len(req.Topics)-1]
			for partition, eo := range partitions {
				reqTopic.Partitions = append(reqTopic.Partitions, kmsg.OffsetCommitRequestTopicPartition{
					Partition:   partition,
					Offset:      eo.Offset,
					LeaderEpoch: eo.Epoch, // KIP-320
					Metadata:    &req.MemberID,
				})
			}
		}

		resp, err := req.RequestWith(commitCtx, g.cl)
		if err != nil {
			onDone(g.cl, req, nil, err)
			return
		}
		g.updateCommitted(req, resp)
		onDone(g.cl, req, resp, nil)
	}()
}
