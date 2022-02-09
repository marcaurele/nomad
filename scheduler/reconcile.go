package scheduler

import (
	"fmt"
	"sort"
	"time"

	"github.com/armon/go-metrics"
	log "github.com/hashicorp/go-hclog"

	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// batchedFailedAllocWindowSize is the window size used
	// to batch up failed allocations before creating an eval
	batchedFailedAllocWindowSize = 5 * time.Second

	// rescheduleWindowSize is the window size relative to
	// current time within which reschedulable allocations are placed.
	// This helps protect against small clock drifts between servers
	rescheduleWindowSize = 1 * time.Second
)

// allocUpdateType takes an existing allocation and a new job definition and
// returns whether the allocation can ignore the change, requires a destructive
// update, or can be inplace updated. If it can be inplace updated, an updated
// allocation that has the new resources and alloc metrics attached will be
// returned.
type allocUpdateType func(existing *structs.Allocation, newJob *structs.Job,
	newTG *structs.TaskGroup) (ignore, destructive bool, updated *structs.Allocation)

// allocReconciler is used to determine the set of allocations that require
// placement, inplace updating or stopping given the job specification and
// existing cluster state. The reconciler should only be used for batch and
// service jobs.
type allocReconciler struct {
	// logger is used to log debug information. Logging should be kept at a
	// minimal here
	logger log.Logger

	// canInplace is used to check if the allocation can be inplace upgraded
	allocUpdateFn allocUpdateType

	// batch marks whether the job is a batch job
	batch bool

	// job is the job being operated on, it may be nil if the job is being
	// stopped via a purge
	job *structs.Job

	// jobID is the ID of the job being operated on. The job may be nil if it is
	// being stopped so we require this separately.
	jobID string

	// oldDeployment is the last deployment for the job
	oldDeployment *structs.Deployment

	// deployment is the current deployment for the job
	deployment *structs.Deployment

	// deploymentPaused marks whether the deployment is paused
	deploymentPaused bool

	// deploymentFailed marks whether the deployment is failed
	deploymentFailed bool

	// taintedNodes contains a map of nodes that are tainted
	taintedNodes map[string]*structs.Node

	// existingAllocs is non-terminal existing allocations
	existingAllocs []*structs.Allocation

	// evalID and evalPriority is the ID and Priority of the evaluation that
	// triggered the reconciler.
	evalID       string
	evalPriority int

	// now is the time used when determining rescheduling eligibility
	// defaults to time.Now, and overridden in unit tests
	now time.Time

	// result is the results of the reconcile. During computation it can be
	// used to store intermediate state
	result *reconcileResults
}

// reconcileResults contains the results of the reconciliation and should be
// applied by the scheduler.
type reconcileResults struct {
	// deployment is the deployment that should be created or updated as a
	// result of scheduling
	deployment *structs.Deployment

	// deploymentUpdates contains a set of deployment updates that should be
	// applied as a result of scheduling
	deploymentUpdates []*structs.DeploymentStatusUpdate

	// place is the set of allocations to place by the scheduler
	place []allocPlaceResult

	// destructiveUpdate is the set of allocations to apply a destructive update to
	destructiveUpdate []allocDestructiveResult

	// inplaceUpdate is the set of allocations to apply an inplace update to
	inplaceUpdate []*structs.Allocation

	// stop is the set of allocations to stop
	stop []allocStopResult

	// attributeUpdates are updates to the allocation that are not from a
	// jobspec change.
	attributeUpdates map[string]*structs.Allocation

	// desiredTGUpdates captures the desired set of changes to make for each
	// task group.
	desiredTGUpdates map[string]*structs.DesiredUpdates

	// desiredFollowupEvals is the map of follow up evaluations to create per task group
	// This is used to create a delayed evaluation for rescheduling failed allocations.
	desiredFollowupEvals map[string][]*structs.Evaluation
}

// delayedRescheduleInfo contains the allocation id and a time when its eligible to be rescheduled.
// this is used to create follow up evaluations
type delayedRescheduleInfo struct {

	// allocID is the ID of the allocation eligible to be rescheduled
	allocID string

	alloc *structs.Allocation

	// rescheduleTime is the time to use in the delayed evaluation
	rescheduleTime time.Time
}

func (r *reconcileResults) GoString() string {
	base := fmt.Sprintf("Total changes: (place %d) (destructive %d) (inplace %d) (stop %d)",
		len(r.place), len(r.destructiveUpdate), len(r.inplaceUpdate), len(r.stop))

	if r.deployment != nil {
		base += fmt.Sprintf("\nCreated Deployment: %q", r.deployment.ID)
	}
	for _, u := range r.deploymentUpdates {
		base += fmt.Sprintf("\nDeployment Update for ID %q: Status %q; Description %q",
			u.DeploymentID, u.Status, u.StatusDescription)
	}
	for tg, u := range r.desiredTGUpdates {
		base += fmt.Sprintf("\nDesired Changes for %q: %#v", tg, u)
	}
	return base
}

// Changes returns the number of total changes
func (r *reconcileResults) Changes() int {
	return len(r.place) + len(r.inplaceUpdate) + len(r.stop)
}

// NewAllocReconciler creates a new reconciler that should be used to determine
// the changes required to bring the cluster state inline with the declared jobspec
func NewAllocReconciler(logger log.Logger, allocUpdateFn allocUpdateType, batch bool,
	jobID string, job *structs.Job, deployment *structs.Deployment,
	existingAllocs []*structs.Allocation, taintedNodes map[string]*structs.Node, evalID string,
	evalPriority int) *allocReconciler {
	return &allocReconciler{
		logger:         logger.Named("reconciler"),
		allocUpdateFn:  allocUpdateFn,
		batch:          batch,
		jobID:          jobID,
		job:            job,
		deployment:     deployment.Copy(),
		existingAllocs: existingAllocs,
		taintedNodes:   taintedNodes,
		evalID:         evalID,
		evalPriority:   evalPriority,
		now:            time.Now(),
		result: &reconcileResults{
			attributeUpdates:     make(map[string]*structs.Allocation),
			desiredTGUpdates:     make(map[string]*structs.DesiredUpdates),
			desiredFollowupEvals: make(map[string][]*structs.Evaluation),
		},
	}
}

// Compute reconciles the existing cluster state and returns the set of changes
// required to converge the job spec and state
func (a *allocReconciler) Compute() *reconcileResults {
	// Create the allocation matrix
	m := newAllocMatrix(a.job, a.existingAllocs)

	a.cancelUnneededDeployments()

	// If we are just stopping a job we do not need to do anything more than
	// stopping all running allocs
	if a.job.Stopped() {
		a.handleStop(m)
		return a.result
	}

	a.computeDeploymentPaused()
	deploymentComplete := a.computeDeploymentComplete(m)
	a.computeDeploymentUpdates(deploymentComplete)

	return a.result
}

func (a *allocReconciler) computeDeploymentComplete(m allocMatrix) bool {
	complete := true
	for group, as := range m {
		groupComplete := a.computeGroup(group, as)
		complete = complete && groupComplete
	}
	return complete
}

func (a *allocReconciler) computeDeploymentUpdates(deploymentComplete bool) {
	// Mark the deployment as complete if possible
	if a.deployment != nil && deploymentComplete {
		if a.job.IsMultiregion() {
			// the unblocking/successful states come after blocked, so we
			// need to make sure we don't revert those states
			if a.deployment.Status != structs.DeploymentStatusUnblocking &&
				a.deployment.Status != structs.DeploymentStatusSuccessful {
				a.result.deploymentUpdates = append(a.result.deploymentUpdates, &structs.DeploymentStatusUpdate{
					DeploymentID:      a.deployment.ID,
					Status:            structs.DeploymentStatusBlocked,
					StatusDescription: structs.DeploymentStatusDescriptionBlocked,
				})
			}
		} else {
			a.result.deploymentUpdates = append(a.result.deploymentUpdates, &structs.DeploymentStatusUpdate{
				DeploymentID:      a.deployment.ID,
				Status:            structs.DeploymentStatusSuccessful,
				StatusDescription: structs.DeploymentStatusDescriptionSuccessful,
			})
		}
	}

	// Set the description of a created deployment
	if d := a.result.deployment; d != nil {
		if d.RequiresPromotion() {
			if d.HasAutoPromote() {
				d.StatusDescription = structs.DeploymentStatusDescriptionRunningAutoPromotion
			} else {
				d.StatusDescription = structs.DeploymentStatusDescriptionRunningNeedsPromotion
			}
		}
	}
}

func (a *allocReconciler) computeDeploymentPaused() {
	if a.deployment != nil {
		a.deploymentPaused = a.deployment.Status == structs.DeploymentStatusPaused ||
			a.deployment.Status == structs.DeploymentStatusPending
		a.deploymentFailed = a.deployment.Status == structs.DeploymentStatusFailed
	}
	if a.deployment == nil {
		// When we create the deployment later, it will be in a pending
		// state. But we also need to tell Compute we're paused, otherwise we
		// make placements on the paused deployment.
		if a.job.IsMultiregion() && !(a.job.IsPeriodic() || a.job.IsParameterized()) {
			a.deploymentPaused = true
		}
	}
}

// cancelUnneededDeployments cancels any deployment that is not needed. If the
// current deployment is not needed the deployment field is set to nil. A deployment
// update will be staged for jobs that should stop or have the wrong version.
// Unneeded deployments include:
// 1. Jobs that are marked for stop, but there is a non-terminal deployment.
// 2. Deployments that are active, but referencing a different job version.
// 3. Deployments that are already successful.
func (a *allocReconciler) cancelUnneededDeployments() {
	// If the job is stopped and there is a non-terminal deployment, cancel it
	if a.job.Stopped() {
		if a.deployment != nil && a.deployment.Active() {
			a.result.deploymentUpdates = append(a.result.deploymentUpdates, &structs.DeploymentStatusUpdate{
				DeploymentID:      a.deployment.ID,
				Status:            structs.DeploymentStatusCancelled,
				StatusDescription: structs.DeploymentStatusDescriptionStoppedJob,
			})
		}

		// Nothing else to do
		a.oldDeployment = a.deployment
		a.deployment = nil
		return
	}

	d := a.deployment
	if d == nil {
		return
	}

	// Check if the deployment is active and referencing an older job and cancel it
	if d.JobCreateIndex != a.job.CreateIndex || d.JobVersion != a.job.Version {
		if d.Active() {
			a.result.deploymentUpdates = append(a.result.deploymentUpdates, &structs.DeploymentStatusUpdate{
				DeploymentID:      a.deployment.ID,
				Status:            structs.DeploymentStatusCancelled,
				StatusDescription: structs.DeploymentStatusDescriptionNewerJob,
			})
		}

		a.oldDeployment = d
		a.deployment = nil
	}

	// Clear it as the current deployment if it is successful
	if d.Status == structs.DeploymentStatusSuccessful {
		a.oldDeployment = d
		a.deployment = nil
	}
}

// handleStop marks all allocations to be stopped, handling the lost case
func (a *allocReconciler) handleStop(m allocMatrix) {
	for group, as := range m {
		as = filterByTerminal(as)
		desiredChanges := new(structs.DesiredUpdates)
		desiredChanges.Stop += a.stopAllocSet(as)
		a.result.desiredTGUpdates[group] = desiredChanges
	}
}

func (a *allocReconciler) stopAllocSet(set allocSet) uint64 {
	untainted, migrate, lost := set.filterByTainted(a.taintedNodes)
	a.markStop(untainted, "", allocNotNeeded)
	a.markStop(migrate, "", allocNotNeeded)
	a.markStop(lost, structs.AllocClientStatusLost, allocLost)
	return uint64(len(untainted) + len(migrate) + len(lost))
}

// markStop is a helper for marking a set of allocation for stop with a
// particular client status and description.
func (a *allocReconciler) markStop(allocs allocSet, clientStatus, statusDescription string) {
	for _, alloc := range allocs {
		a.result.stop = append(a.result.stop, allocStopResult{
			alloc:             alloc,
			clientStatus:      clientStatus,
			statusDescription: statusDescription,
		})
	}
}

// markDelayed does markStop, but optionally includes a FollowupEvalID so that we can update
// the stopped alloc with its delayed rescheduling evalID
func (a *allocReconciler) markDelayed(allocs allocSet, clientStatus, statusDescription string, followupEvals map[string]string) {
	for _, alloc := range allocs {
		a.result.stop = append(a.result.stop, allocStopResult{
			alloc:             alloc,
			clientStatus:      clientStatus,
			statusDescription: statusDescription,
			followupEvalID:    followupEvals[alloc.ID],
		})
	}
}

// computeGroup reconciles state for a particular task group. It returns whether
// the deployment it is for is complete with regards to the task group.
func (a *allocReconciler) computeGroup(groupName string, all allocSet) bool {
	// Create the desired update object for the group
	desiredChanges := new(structs.DesiredUpdates)
	a.result.desiredTGUpdates[groupName] = desiredChanges

	// Get the task group. The task group may be nil if the job was updates such
	// that the task group no longer exists
	tg := a.job.LookupTaskGroup(groupName)

	// If the task group is nil, then the task group has been removed so all we
	// need to do is stop everything
	if tg == nil {
		desiredChanges.Stop += a.stopAllocSet(all)
		return true
	}

	dstate, existingDeployment := a.initializeDeploymentState(groupName, tg)

	// Filter allocations that do not need to be considered because they are
	// from an older job version and are terminal.
	all, ignore := a.filterOldTerminalAllocs(all)
	desiredChanges.Ignore += uint64(len(ignore))

	canaries, all := a.cancelUnneededCanaries(all, desiredChanges)

	// Determine what set of allocations are on tainted nodes
	untainted, migrate, lost := all.filterByTainted(a.taintedNodes)

	// Determine what set of terminal allocations need to be rescheduled
	untainted, rescheduleNow, rescheduleLater := untainted.filterByRescheduleable(a.batch, a.now, a.evalID, a.deployment)

	// Find delays for any lost allocs that have stop_after_client_disconnect
	lostLater := lost.delayByStopAfterClientDisconnect()
	lostLaterEvals := a.computeLostLaterEvals(lostLater, all, tg.Name)

	// Create batched follow up evaluations for allocations that are
	// reschedulable later and mark the allocations for in place updating
	a.computeRescheduleLater(rescheduleLater, all, tg.Name)

	// Create a structure for choosing names. Seed with the taken names
	// which is the union of untainted, rescheduled, allocs on migrating
	// nodes, and allocs on down nodes (includes canaries)
	nameIndex := newAllocNameIndex(a.jobID, groupName, tg.Count, untainted.union(migrate, rescheduleNow, lost))

	// Stop any unneeded allocations and update the untainted set to not
	// include stopped allocations.
	isCanarying := dstate != nil && dstate.DesiredCanaries != 0 && !dstate.Promoted
	stop := a.computeStop(tg, nameIndex, untainted, migrate, lost, canaries, isCanarying, lostLaterEvals)
	desiredChanges.Stop += uint64(len(stop))
	untainted = untainted.difference(stop)

	// Do inplace upgrades where possible and capture the set of upgrades that
	// need to be done destructively.
	ignore, inplace, destructive := a.computeUpdates(tg, untainted)
	desiredChanges.Ignore += uint64(len(ignore))
	desiredChanges.InPlaceUpdate += uint64(len(inplace))
	if !existingDeployment {
		dstate.DesiredTotal += len(destructive) + len(inplace)
	}

	// Remove the canaries now that we have handled rescheduling so that we do
	// not consider them when making placement decisions.
	if isCanarying {
		untainted = untainted.difference(canaries)
	}

	strategy, canariesNeeded := a.computeCanaries(tg, dstate, destructive, canaries, desiredChanges, nameIndex)

	// Determine how many non-canary allocs we can place
	isCanarying = dstate != nil && dstate.DesiredCanaries != 0 && !dstate.Promoted
	underProvisionedBy := a.computeUnderProvisionedBy(tg, untainted, destructive, migrate, isCanarying)

	// Place if:
	// * The deployment is not paused or failed
	// * Not placing any canaries
	// * If there are any canaries that they have been promoted
	// * There is no delayed stop_after_client_disconnect alloc, which delays scheduling for the whole group
	// * An alloc was lost
	var place []allocPlaceResult
	if len(lostLater) == 0 {
		place = a.computePlacements(tg, nameIndex, untainted, migrate, rescheduleNow, lost, isCanarying)
		if !existingDeployment {
			dstate.DesiredTotal += len(place)
		}
	}

	// deploymentPlaceReady tracks whether the deployment is in a state where
	// placements can be made without any other consideration.
	deploymentPlaceReady := !a.deploymentPaused && !a.deploymentFailed && !isCanarying

	underProvisionedBy = a.computeReplacements(deploymentPlaceReady, desiredChanges, place, rescheduleNow, lost, underProvisionedBy)

	if deploymentPlaceReady {
		a.computeDestructiveUpdates(destructive, underProvisionedBy, desiredChanges, tg)
	} else {
		desiredChanges.Ignore += uint64(len(destructive))
	}

	// Migrate all the allocations
	desiredChanges.Migrate += uint64(len(migrate))
	for _, alloc := range migrate.nameOrder() {
		a.result.stop = append(a.result.stop, allocStopResult{
			alloc:             alloc,
			statusDescription: allocMigrating,
		})
		a.result.place = append(a.result.place, allocPlaceResult{
			name:          alloc.Name,
			canary:        alloc.DeploymentStatus.IsCanary(),
			taskGroup:     tg,
			previousAlloc: alloc,

			downgradeNonCanary: isCanarying && !alloc.DeploymentStatus.IsCanary(),
			minJobVersion:      alloc.Job.Version,
		})
	}

	// Create new deployment if:
	// 1. Updating a job specification
	// 2. No running allocations (first time running a job)
	updatingSpec := len(destructive) != 0 || len(a.result.inplaceUpdate) != 0
	hadRunning := false
	for _, alloc := range all {
		if alloc.Job.Version == a.job.Version && alloc.Job.CreateIndex == a.job.CreateIndex {
			hadRunning = true
			break
		}
	}

	// Create a new deployment if necessary
	if !existingDeployment && !strategy.IsEmpty() && dstate.DesiredTotal != 0 && (!hadRunning || updatingSpec) {
		// A previous group may have made the deployment already
		if a.deployment == nil {
			a.deployment = structs.NewDeployment(a.job, a.evalPriority)
			// in multiregion jobs, most deployments start in a pending state
			if a.job.IsMultiregion() && !(a.job.IsPeriodic() && a.job.IsParameterized()) {
				a.deployment.Status = structs.DeploymentStatusPending
				a.deployment.StatusDescription = structs.DeploymentStatusDescriptionPendingForPeer
			}
			a.result.deployment = a.deployment
		}

		// Attach the groups deployment state to the deployment
		a.deployment.TaskGroups[groupName] = dstate
	}

	// deploymentComplete is whether the deployment is complete which largely
	// means that no placements were made or desired to be made
	deploymentComplete := len(destructive)+len(inplace)+len(place)+len(migrate)+len(rescheduleNow)+len(rescheduleLater) == 0 && !canariesNeeded

	// Final check to see if the deployment is complete is to ensure everything
	// is healthy
	if deploymentComplete && a.deployment != nil {
		if dstate, ok := a.deployment.TaskGroups[groupName]; ok {
			if dstate.HealthyAllocs < helper.IntMax(dstate.DesiredTotal, dstate.DesiredCanaries) || // Make sure we have enough healthy allocs
				(dstate.DesiredCanaries > 0 && !dstate.Promoted) { // Make sure we are promoted if we have canaries
				deploymentComplete = false
			}
		}
	}

	return deploymentComplete
}

func (a *allocReconciler) initializeDeploymentState(group string, tg *structs.TaskGroup) (*structs.DeploymentState, bool) {
	var dstate *structs.DeploymentState
	existingDeployment := false

	if a.deployment != nil {
		dstate, existingDeployment = a.deployment.TaskGroups[group]
	}

	if !existingDeployment {
		dstate = &structs.DeploymentState{}
		if !tg.Update.IsEmpty() {
			dstate.AutoRevert = tg.Update.AutoRevert
			dstate.AutoPromote = tg.Update.AutoPromote
			dstate.ProgressDeadline = tg.Update.ProgressDeadline
		}
	}

	return dstate, existingDeployment
}

func (a *allocReconciler) computeCanaries(tg *structs.TaskGroup, dstate *structs.DeploymentState,
	destructive, canaries allocSet, desiredChanges *structs.DesiredUpdates, nameIndex *allocNameIndex) (*structs.UpdateStrategy, bool) {

	// If we have destructive updates, and have fewer canaries than is
	// desired, we need to create canaries.
	strategy := tg.Update
	canariesPromoted := dstate != nil && dstate.Promoted
	canariesNeeded := len(destructive) != 0 &&
		strategy != nil &&
		len(canaries) < strategy.Canary &&
		!canariesPromoted

	if canariesNeeded {
		dstate.DesiredCanaries = strategy.Canary
	}

	if canariesNeeded && !a.deploymentPaused && !a.deploymentFailed {
		desiredChanges.Canary += uint64(strategy.Canary - len(canaries))
		for _, name := range nameIndex.NextCanaries(uint(desiredChanges.Canary), canaries, destructive) {
			a.result.place = append(a.result.place, allocPlaceResult{
				name:      name,
				canary:    true,
				taskGroup: tg,
			})
		}
	}
	return strategy, canariesNeeded
}

// filterOldTerminalAllocs filters allocations that should be ignored since they
// are allocations that are terminal from a previous job version.
func (a *allocReconciler) filterOldTerminalAllocs(all allocSet) (filtered, ignore allocSet) {
	if !a.batch {
		return all, nil
	}

	filtered = filtered.union(all)
	ignored := make(map[string]*structs.Allocation)

	// Ignore terminal batch jobs from older versions
	for id, alloc := range filtered {
		older := alloc.Job.Version < a.job.Version || alloc.Job.CreateIndex < a.job.CreateIndex
		if older && alloc.TerminalStatus() {
			delete(filtered, id)
			ignored[id] = alloc
		}
	}

	return filtered, ignored
}

// cancelUnneededCanaries handles the canaries for the group by stopping the
// unneeded ones and returning the current set of canaries and the updated total
// set of allocs for the group
func (a *allocReconciler) cancelUnneededCanaries(all allocSet, desiredChanges *structs.DesiredUpdates) (canaries, newAll allocSet) {
	// Stop any canary from an older deployment or from a failed one
	var stop []string

	// Cancel any non-promoted canaries from the older deployment
	if a.oldDeployment != nil {
		for _, dstate := range a.oldDeployment.TaskGroups {
			if !dstate.Promoted {
				stop = append(stop, dstate.PlacedCanaries...)
			}
		}
	}

	// Cancel any non-promoted canaries from a failed deployment
	if a.deployment != nil && a.deployment.Status == structs.DeploymentStatusFailed {
		for _, dstate := range a.deployment.TaskGroups {
			if !dstate.Promoted {
				stop = append(stop, dstate.PlacedCanaries...)
			}
		}
	}

	// stopSet is the allocSet that contains the canaries we desire to stop from
	// above.
	stopSet := all.fromKeys(stop)
	a.markStop(stopSet, "", allocNotNeeded)
	desiredChanges.Stop += uint64(len(stopSet))
	all = all.difference(stopSet)

	// Capture our current set of canaries and handle any migrations that are
	// needed by just stopping them.
	if a.deployment != nil {
		var canaryIDs []string
		for _, dstate := range a.deployment.TaskGroups {
			canaryIDs = append(canaryIDs, dstate.PlacedCanaries...)
		}

		canaries = all.fromKeys(canaryIDs)
		untainted, migrate, lost := canaries.filterByTainted(a.taintedNodes)
		a.markStop(migrate, "", allocMigrating)
		a.markStop(lost, structs.AllocClientStatusLost, allocLost)

		canaries = untainted
		all = all.difference(migrate, lost)
	}

	return canaries, all
}

// computeUnderProvisionedBy returns the number of allocs that still need to be
// placed for a particular group. The inputs are the group definition, the untainted,
// destructive, and migrate allocation sets, and whether we are in a canary state.
// The following rules are applied:
// * If no update strategy, or nothing is migrating, being replaced, or reconnecting,
//   allow as many as defined in group.Count
// * If the deployment is paused, failed, or canarying, allow no placements
// * If the deployment is nil, allow MaxParallel placements
// * If the deployment is not nil, but has unhealthy allocs, allow no placements
// * If the deployment is not nil, and all allocs are healthy, allow MaxParallel
//   minus the number of healthy allocs placements
// * If the calculated limit is less than zero, return 0
func (a *allocReconciler) computeUnderProvisionedBy(group *structs.TaskGroup, untainted, destructive, migrate allocSet, isCanarying bool) int {
	// If there is no update strategy or deployment for the group we can deploy
	// as many as the group has
	if group.Update.IsEmpty() || len(destructive)+len(migrate) == 0 {
		return group.Count
	}

	// If the deployment is paused or failed, or we have un-promoted canaries
	// do not create anything else
	if a.deploymentPaused ||
		a.deploymentFailed ||
		isCanarying {
		return 0
	}

	// If we have been promoted or there are no canaries, the limit is the
	// configured MaxParallel minus any outstanding non-healthy alloc for the deployment
	underProvisionedBy := group.Update.MaxParallel
	if a.deployment != nil {
		partOf, _ := untainted.filterByDeployment(a.deployment.ID)
		for _, alloc := range partOf {
			// An unhealthy allocation means nothing else should be happening.
			if alloc.DeploymentStatus.IsUnhealthy() {
				return 0
			}

			if !alloc.DeploymentStatus.IsHealthy() {
				underProvisionedBy--
			}
		}
	}

	// The limit can be less than zero in the case that the job was changed such
	// that it required destructive changes and the count was scaled up.
	if underProvisionedBy < 0 {
		return 0
	}

	return underProvisionedBy
}

// computePlacements returns the set of allocations to place given the group
// definition, the set of untainted, migrating and reschedule allocations for the group.
//
// Placements will meet or exceed group count.
func (a *allocReconciler) computePlacements(group *structs.TaskGroup,
	nameIndex *allocNameIndex, untainted, migrate, reschedule, lost allocSet,
	isCanarying bool) []allocPlaceResult {

	// Add rescheduled placement results
	var place []allocPlaceResult
	for _, alloc := range reschedule {
		place = append(place, allocPlaceResult{
			name:          alloc.Name,
			taskGroup:     group,
			previousAlloc: alloc,
			reschedule:    true,
			canary:        alloc.DeploymentStatus.IsCanary(),

			downgradeNonCanary: isCanarying && !alloc.DeploymentStatus.IsCanary(),
			minJobVersion:      alloc.Job.Version,
			lost:               false,
		})
	}

	// Add replacements for lost allocs up to group.Count
	existing := len(untainted) + len(migrate) + len(reschedule)

	for _, alloc := range lost {
		if existing >= group.Count {
			// Reached desired count, do not replace remaining lost
			// allocs
			break
		}

		existing++
		place = append(place, allocPlaceResult{
			name:               alloc.Name,
			taskGroup:          group,
			previousAlloc:      alloc,
			reschedule:         false,
			canary:             alloc.DeploymentStatus.IsCanary(),
			downgradeNonCanary: isCanarying && !alloc.DeploymentStatus.IsCanary(),
			minJobVersion:      alloc.Job.Version,
			lost:               true,
		})
	}

	// Add remaining placement results
	if existing < group.Count {
		for _, name := range nameIndex.Next(uint(group.Count - existing)) {
			place = append(place, allocPlaceResult{
				name:               name,
				taskGroup:          group,
				downgradeNonCanary: isCanarying,
			})
		}
	}

	return place
}

// computeReplacements either applies the placements calculated by computePlacements,
// or computes more placements based on whether the deployment is ready for placement
// and if the placement is already rescheduling or part of a failed deployment.
// The input deploymentPlaceReady is calculated as the deployment is not paused, failed, or canarying
// The following rules are applied:
// * If the deployment is place ready, apply all placements and return
// * If not place ready, check if any allocs have been lost
// * If allocs have been lost, determine the number of replacements that are needed.
// * Add placements to the result for the lost allocs.
// * If there are failed allocations, inspect them.
// * If the placement is rescheduling, and not part of a failed deployment, add
//   to the place set, and add the previous alloc to the stop set.
// * Return the number of allocs still needed.
func (a *allocReconciler) computeReplacements(deploymentPlaceReady bool, desiredChanges *structs.DesiredUpdates,
	place []allocPlaceResult, rescheduleNow, lost allocSet, underProvisionedBy int) int {

	if deploymentPlaceReady {
		desiredChanges.Place += uint64(len(place))
		// This relies on the computePlacements having built this set, which in
		// turn relies on len(lostLater) == 0.
		a.result.place = append(a.result.place, place...)
		a.markStop(rescheduleNow, "", allocRescheduled)
		desiredChanges.Stop += uint64(len(rescheduleNow))

		min := helper.IntMin(len(place), underProvisionedBy)
		underProvisionedBy -= min
	} else if !deploymentPlaceReady {
		// We do not want to place additional allocations but in the case we
		// have lost allocations or allocations that require rescheduling now,
		// we do so regardless to avoid odd user experiences.
		if len(lost) != 0 {
			allowed := helper.IntMin(len(lost), len(place))
			desiredChanges.Place += uint64(allowed)
			a.result.place = append(a.result.place, place[:allowed]...)
		}

		// Handle rescheduling of failed allocations even if the deployment is
		// failed. We do not reschedule if the allocation is part of the failed
		// deployment.
		if now := len(rescheduleNow); now != 0 {
			for _, p := range place {
				prev := p.PreviousAllocation()
				if p.IsRescheduling() && !(a.deploymentFailed && prev != nil && a.deployment.ID == prev.DeploymentID) {
					a.result.place = append(a.result.place, p)
					desiredChanges.Place++

					a.result.stop = append(a.result.stop, allocStopResult{
						alloc:             prev,
						statusDescription: allocRescheduled,
					})
					desiredChanges.Stop++
				}
			}
		}
	}

	return underProvisionedBy
}

func (a *allocReconciler) computeDestructiveUpdates(destructive allocSet, underProvisionedBy int,
	desiredChanges *structs.DesiredUpdates, tg *structs.TaskGroup) {

	// Do all destructive updates
	min := helper.IntMin(len(destructive), underProvisionedBy)
	desiredChanges.DestructiveUpdate += uint64(min)
	desiredChanges.Ignore += uint64(len(destructive) - min)
	for _, alloc := range destructive.nameOrder()[:min] {
		a.result.destructiveUpdate = append(a.result.destructiveUpdate, allocDestructiveResult{
			placeName:             alloc.Name,
			placeTaskGroup:        tg,
			stopAlloc:             alloc,
			stopStatusDescription: allocUpdating,
		})
	}
}

// computeStop returns the set of allocations that are marked for stopping given
// the group definition, the set of allocations in various states and whether we
// are canarying.
func (a *allocReconciler) computeStop(group *structs.TaskGroup, nameIndex *allocNameIndex,
	untainted, migrate, lost, canaries allocSet, isCanarying bool, followupEvals map[string]string) allocSet {

	// Mark all lost allocations for stop.
	var stop allocSet
	stop = stop.union(lost)
	a.markDelayed(lost, structs.AllocClientStatusLost, allocLost, followupEvals)

	// If we are still deploying or creating canaries, don't stop them
	if isCanarying {
		untainted = untainted.difference(canaries)
	}

	// Hot path the nothing to do case
	remove := len(untainted) + len(migrate) - group.Count
	if remove <= 0 {
		return stop
	}

	// Filter out any terminal allocations from the untainted set
	// This is so that we don't try to mark them as stopped redundantly
	untainted = filterByTerminal(untainted)

	// Prefer stopping any alloc that has the same name as the canaries if we
	// are promoted
	if !isCanarying && len(canaries) != 0 {
		canaryNames := canaries.nameSet()
		for id, alloc := range untainted.difference(canaries) {
			if _, match := canaryNames[alloc.Name]; match {
				stop[id] = alloc
				a.result.stop = append(a.result.stop, allocStopResult{
					alloc:             alloc,
					statusDescription: allocNotNeeded,
				})
				delete(untainted, id)

				remove--
				if remove == 0 {
					return stop
				}
			}
		}
	}

	// Prefer selecting from the migrating set before stopping existing allocs
	if len(migrate) != 0 {
		migratingNames := newAllocNameIndex(a.jobID, group.Name, group.Count, migrate)
		removeNames := migratingNames.Highest(uint(remove))
		for id, alloc := range migrate {
			if _, match := removeNames[alloc.Name]; !match {
				continue
			}
			a.result.stop = append(a.result.stop, allocStopResult{
				alloc:             alloc,
				statusDescription: allocNotNeeded,
			})
			delete(migrate, id)
			stop[id] = alloc
			nameIndex.UnsetIndex(alloc.Index())

			remove--
			if remove == 0 {
				return stop
			}
		}
	}

	// Select the allocs with the highest count to remove
	removeNames := nameIndex.Highest(uint(remove))
	for id, alloc := range untainted {
		if _, ok := removeNames[alloc.Name]; ok {
			stop[id] = alloc
			a.result.stop = append(a.result.stop, allocStopResult{
				alloc:             alloc,
				statusDescription: allocNotNeeded,
			})
			delete(untainted, id)

			remove--
			if remove == 0 {
				return stop
			}
		}
	}

	// It is possible that we didn't stop as many as we should have if there
	// were allocations with duplicate names.
	for id, alloc := range untainted {
		stop[id] = alloc
		a.result.stop = append(a.result.stop, allocStopResult{
			alloc:             alloc,
			statusDescription: allocNotNeeded,
		})
		delete(untainted, id)

		remove--
		if remove == 0 {
			return stop
		}
	}

	return stop
}

// computeUpdates determines which allocations for the passed group require
// updates. Three groups are returned:
// 1. Those that require no upgrades
// 2. Those that can be upgraded in-place. These are added to the results
// automatically since the function contains the correct state to do so,
// 3. Those that require destructive updates
func (a *allocReconciler) computeUpdates(group *structs.TaskGroup, untainted allocSet) (ignore, inplace, destructive allocSet) {
	// Determine the set of allocations that need to be updated
	ignore = make(map[string]*structs.Allocation)
	inplace = make(map[string]*structs.Allocation)
	destructive = make(map[string]*structs.Allocation)

	for _, alloc := range untainted {
		ignoreChange, destructiveChange, inplaceAlloc := a.allocUpdateFn(alloc, a.job, group)
		if ignoreChange {
			ignore[alloc.ID] = alloc
		} else if destructiveChange {
			destructive[alloc.ID] = alloc
		} else {
			inplace[alloc.ID] = alloc
			a.result.inplaceUpdate = append(a.result.inplaceUpdate, inplaceAlloc)
		}
	}

	return
}

// computeRescheduleLater creates batched followup evaluations with the WaitUntil field
// set for allocations that are eligible to be rescheduled later, and marks the alloc with
// the followupEvalID
func (a *allocReconciler) computeRescheduleLater(rescheduleLater []*delayedRescheduleInfo, all allocSet, tgName string) {
	// followupEvals are created in the same way as for delayed lost allocs
	allocIDToFollowupEvalID := a.computeLostLaterEvals(rescheduleLater, all, tgName)

	// Initialize the annotations
	if len(allocIDToFollowupEvalID) != 0 && a.result.attributeUpdates == nil {
		a.result.attributeUpdates = make(map[string]*structs.Allocation)
	}

	// Create updates that will be applied to the allocs to mark the FollowupEvalID
	for allocID, evalID := range allocIDToFollowupEvalID {
		existingAlloc := all[allocID]
		updatedAlloc := existingAlloc.Copy()
		updatedAlloc.FollowupEvalID = evalID
		a.result.attributeUpdates[updatedAlloc.ID] = updatedAlloc
	}
}

// computeLostLaterEvals creates batched followup evaluations with the WaitUntil field set for
// lost allocations. followupEvals are appended to a.result as a side effect, we return a
// map of alloc IDs to their followupEval IDs
func (a *allocReconciler) computeLostLaterEvals(rescheduleLater []*delayedRescheduleInfo, all allocSet, tgName string) map[string]string {
	if len(rescheduleLater) == 0 {
		return map[string]string{}
	}

	// Sort by time
	sort.Slice(rescheduleLater, func(i, j int) bool {
		return rescheduleLater[i].rescheduleTime.Before(rescheduleLater[j].rescheduleTime)
	})

	var evals []*structs.Evaluation
	nextReschedTime := rescheduleLater[0].rescheduleTime
	allocIDToFollowupEvalID := make(map[string]string, len(rescheduleLater))

	// Create a new eval for the first batch
	eval := &structs.Evaluation{
		ID:                uuid.Generate(),
		Namespace:         a.job.Namespace,
		Priority:          a.evalPriority,
		Type:              a.job.Type,
		TriggeredBy:       structs.EvalTriggerRetryFailedAlloc,
		JobID:             a.job.ID,
		JobModifyIndex:    a.job.ModifyIndex,
		Status:            structs.EvalStatusPending,
		StatusDescription: reschedulingFollowupEvalDesc,
		WaitUntil:         nextReschedTime,
	}
	evals = append(evals, eval)

	for _, allocReschedInfo := range rescheduleLater {
		if allocReschedInfo.rescheduleTime.Sub(nextReschedTime) < batchedFailedAllocWindowSize {
			allocIDToFollowupEvalID[allocReschedInfo.allocID] = eval.ID
		} else {
			// Start a new batch
			nextReschedTime = allocReschedInfo.rescheduleTime
			// Create a new eval for the new batch
			eval = &structs.Evaluation{
				ID:             uuid.Generate(),
				Namespace:      a.job.Namespace,
				Priority:       a.evalPriority,
				Type:           a.job.Type,
				TriggeredBy:    structs.EvalTriggerRetryFailedAlloc,
				JobID:          a.job.ID,
				JobModifyIndex: a.job.ModifyIndex,
				Status:         structs.EvalStatusPending,
				WaitUntil:      nextReschedTime,
			}
			evals = append(evals, eval)
			// Set the evalID for the first alloc in this new batch
			allocIDToFollowupEvalID[allocReschedInfo.allocID] = eval.ID
		}
		emitRescheduleInfo(allocReschedInfo.alloc, eval)
	}

	a.result.desiredFollowupEvals[tgName] = evals

	return allocIDToFollowupEvalID
}

// emitRescheduleInfo emits metrics about the reschedule decision of an evaluation. If a followup evaluation is
// provided, the waitUntil time is emitted.
func emitRescheduleInfo(alloc *structs.Allocation, followupEval *structs.Evaluation) {
	// Emit short-lived metrics data point. Note, these expire and stop emitting after about a minute.
	baseMetric := []string{"scheduler", "allocs", "reschedule"}
	labels := []metrics.Label{
		{Name: "alloc_id", Value: alloc.ID},
		{Name: "job", Value: alloc.JobID},
		{Name: "namespace", Value: alloc.Namespace},
		{Name: "task_group", Value: alloc.TaskGroup},
	}
	if followupEval != nil {
		labels = append(labels, metrics.Label{Name: "followup_eval_id", Value: followupEval.ID})
		metrics.SetGaugeWithLabels(append(baseMetric, "wait_until"), float32(followupEval.WaitUntil.Unix()), labels)
	}
	attempted, availableAttempts := alloc.RescheduleInfo()
	metrics.SetGaugeWithLabels(append(baseMetric, "attempted"), float32(attempted), labels)
	metrics.SetGaugeWithLabels(append(baseMetric, "limit"), float32(availableAttempts), labels)
}
