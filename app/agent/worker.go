// Package agent is the part of Henosis that runs in the background: it
// watches the linked project folders and FL Studio, and keeps each folder and
// the server in step without being asked.
package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"henosis/config"
	"henosis/core"
	"henosis/engine"
	"henosis/fl"
	"henosis/flp"
)

// Timing holds every delay the agent uses. Tests shorten them.
type Timing struct {
	Quiet          time.Duration // files must be unchanged this long before they are synced
	Poll           time.Duration // how often the server is asked for news
	Heartbeat      time.Duration // how often a held lock is renewed
	CloseDelay     time.Duration // a project must look closed this long before it counts as closed
	HoldAfterClose time.Duration // a lock is given back this long after closing even if an upload keeps failing
	FullCheck      time.Duration // how often an idle folder is rescanned in full
	RetryMin       time.Duration // first wait after a failure
	RetryMax       time.Duration // longest wait after repeated failures
	TrashAge       time.Duration // how long files replaced by a pull are kept in .henosis/trash
}

// DefaultTiming is what the app uses.
func DefaultTiming() Timing {
	return Timing{
		Quiet: 2 * time.Second, Poll: 5 * time.Second, Heartbeat: 30 * time.Second,
		CloseDelay: 3 * time.Second, HoldAfterClose: 2 * time.Minute, FullCheck: 5 * time.Minute,
		RetryMin: 5 * time.Second, RetryMax: time.Minute, TrashAge: 7 * 24 * time.Hour,
	}
}

// ProjectAPI is what a worker needs from the server for one project.
// client.ProjectRemote implements it.
type ProjectAPI interface {
	engine.Remote
	engine.Locker
	Heartbeat() error
	View() (core.ProjectView, error)
	RequestLock() error
	ForceRelease() error
	Comments() ([]core.Comment, error)
	AddComment(snapshotID, text string) (core.Comment, error)
}

// Status is everything the status page shows about one project.
type Status struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Folder    string `json:"folder"`

	// State is one word for the icon: synced, syncing, editing, waiting,
	// attention, offline or problem.
	State    string `json:"state"`
	Headline string `json:"headline"`
	Detail   string `json:"detail"`
	Busy     string `json:"busy,omitempty"`

	Online          bool     `json:"online"`
	OpenInFL        bool     `json:"open_in_fl"`
	Shared          bool     `json:"shared"`
	Role            string   `json:"role"`
	Members         []string `json:"members"`
	HoldingLock     bool     `json:"holding_lock"`
	LockHolder      string   `json:"lock_holder,omitempty"`       // another member who holds the lock
	LockRequestedBy string   `json:"lock_requested_by,omitempty"` // a member asking this device for the lock
	Yielded         bool     `json:"yielded"`
	UnsentChanges   bool     `json:"unsent_changes"`
	NewerOnServer   bool     `json:"newer_on_server"`
	ConfirmDelete   string   `json:"confirm_delete,omitempty"`
	Conflicts       []string `json:"conflicts"`
	OutsideSamples  []string `json:"outside_samples"` // on this computer, outside the folder, and not copied in (a name clash)
	MissingSamples  []string `json:"missing_samples"` // used by the project, not on this computer at all
	RetentionDays   int      `json:"retention_days"`
	LastSync        string   `json:"last_sync,omitempty"`
	LastActivity    string   `json:"last_activity,omitempty"`
	NextNote        string   `json:"next_note,omitempty"` // the note that goes with the next save
	Gone            bool     `json:"gone"`                // the project is not in the session any more
	AutoSend        bool     `json:"auto_send"`
	AutoUpdate      bool     `json:"auto_update"`
}

type flpEntry struct {
	st   stamp
	info flp.Info
	ok   bool
}

// Worker keeps one linked folder in step with its project on the server.
// Tick does one round of work; the agent calls it about once a second.
type Worker struct {
	Link config.Link

	api    ProjectAPI
	me     core.UserView
	device string
	det    fl.Detector
	now    func() time.Time
	timing Timing
	log    func(level, format string, args ...any)

	mu      sync.Mutex // serialises Tick and the user's actions
	eng     *engine.Engine
	tracker fl.Tracker

	view      folderView
	seen      bool // view holds a real observation
	changedAt time.Time
	syncedSum uint64
	synced    bool // syncedSum is the folder as it was last in step with the server
	lastFull  time.Time
	flps      map[string]flpEntry
	finder    sampleFinder

	project    core.ProjectView
	known      bool // project holds a real answer from the server
	polledAt   time.Time
	pollNow    bool
	online     bool
	gone       bool // the project does not exist for this account any more
	authFailed bool

	open        bool
	closingAt   time.Time // when the project first looked closed
	closedAt    time.Time // when it was last decided to be closed
	holding     bool
	heartbeatAt time.Time
	lockRetryAt time.Time
	yielded     bool // the user handed the lock over; do not take it back unasked

	detectFailed bool
	wasOnline    bool
	pollLogged   bool
	lastHeadline string

	autoSend   bool // upload local changes automatically
	autoUpdate bool // download new versions automatically
	sendOnce   bool // one-shot: send now even if auto-send is off
	updateOnce bool // one-shot: update now even if auto-update is off

	nextNote        string // what the user wrote about the next save
	allowMassDelete bool
	needsConfirm    string
	retryAt         time.Time
	failures        int
	problem         string

	lastSync     time.Time
	lastActivity string

	statusMu sync.Mutex
	status   Status
	busy     string
}

// NewWorker makes a worker for one link. Nothing happens until Tick is called.
func NewWorker(link config.Link, api ProjectAPI, me core.UserView, device string, det fl.Detector, timing Timing, now func() time.Time) *Worker {
	w := &Worker{
		Link: link, api: api, me: me, device: device, det: det, now: now, timing: timing,
		log:        func(string, string, ...any) {},
		flps:       map[string]flpEntry{},
		autoSend:   !link.NoAutoSend,
		autoUpdate: !link.NoAutoUpdate,
	}
	w.eng = engine.Open(link.Folder, sessionRemote{ProjectAPI: api, w: w}, me.Name)
	w.eng.BeforeApply = w.beforeApply
	w.status = Status{ProjectID: link.ProjectID, Name: link.Name, Folder: link.Folder, State: "syncing", Headline: "Starting"}
	return w
}

// sessionRemote is the engine's view of the server. While this device holds
// the project's lock for an FL Studio session, commits use it; otherwise the
// lock is taken just for the commit.
type sessionRemote struct {
	ProjectAPI
	w *Worker
}

func (s sessionRemote) Commit(req core.CommitRequest) (core.CommitResult, error) {
	if s.w.holding {
		return s.ProjectAPI.Commit(req)
	}
	return engine.AutoLock{Remote: s.ProjectAPI, Locker: s.ProjectAPI}.Commit(req)
}

// beforeApply runs right before a pull changes project files. It looks at FL
// Studio once more: a project that was opened while the download ran must
// not change underneath it.
func (w *Worker) beforeApply() error {
	state, err := w.det.Detect()
	if err != nil {
		return nil
	}
	names, titles := w.flpNames()
	if w.tracker.Update(state, names, titles, false) {
		w.open, w.closingAt = true, time.Time{}
		return errors.New("the project was just opened in FL Studio")
	}
	return nil
}

// Status returns the latest published status.
func (w *Worker) Status() Status {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	st := w.status
	st.Busy = w.busy
	return st
}

func (w *Worker) setBusy(note string) {
	w.statusMu.Lock()
	w.busy = note
	w.statusMu.Unlock()
}

// Tick does one round: look at the folder, at FL Studio and at the server,
// then do whatever is due.
func (w *Worker) Tick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tick()
	w.publish()
}

func (w *Worker) dirty() bool { return !w.synced || w.view.Sum != w.syncedSum }

// otherHolder names another member (or another device of this user) that
// holds a live lock.
func (w *Worker) otherHolder() string {
	lock := w.project.Lock
	if lock == nil || lock.Stale || (lock.User.ID == w.me.ID && lock.Device == w.device) {
		return ""
	}
	if lock.User.ID == w.me.ID {
		return "your other computer (" + lock.Device + ")"
	}
	return lock.User.Name
}

func (w *Worker) tick() {
	now := w.now()

	// 1. The folder.
	view := viewFolder(w.Link.Folder)
	flpWritten := false
	if !w.seen || view.Sum != w.view.Sum || view.Saving != w.view.Saving || view.Unstable {
		if w.seen {
			flpWritten = flpsDiffer(view.FLPs, w.view.FLPs) || (w.view.Saving && !view.Saving)
			if view.Sum != w.view.Sum {
				// New edits: a pending question may no longer apply, and a
				// failed attempt deserves a fresh try.
				w.needsConfirm, w.retryAt = "", time.Time{}
			}
		}
		w.changedAt = now
	}
	w.view, w.seen = view, true
	if view.Missing {
		if w.holding {
			w.release()
		}
		return
	}
	quiet := !view.Saving && !view.Unstable && now.Sub(w.changedAt) >= w.timing.Quiet
	if quiet {
		w.refreshFLPInfo()
		// A sample the project uses from elsewhere on this computer (a
		// sample pack in Downloads, say) is copied next to the project
		// file, where FL Studio looks for it, so it travels with the
		// project. Nothing is overwritten; the copies are synced as usual.
		if result, err := w.collect(); err != nil {
			w.log("error", "%s: could not copy a sample into the project: %v", w.Link.Name, err)
		} else if len(result.Copied) > 0 {
			w.log("info", "%s: copied %d sample(s) into the project so your friends get them: %s", w.Link.Name, len(result.Copied), strings.Join(result.Copied, ", "))
		}
	}

	// 2. FL Studio. If it cannot be observed, the last answer stands.
	state, detectErr := w.det.Detect()
	if detectErr != nil && !w.detectFailed {
		w.log("error", "%s: cannot look for FL Studio: %v", w.Link.Name, detectErr)
	}
	w.detectFailed = detectErr != nil
	if detectErr == nil {
		names, titles := w.flpNames()
		if w.tracker.Update(state, names, titles, flpWritten) {
			if !w.open {
				w.log("info", "%s: opened in FL Studio (windows: %q)", w.Link.Name, state.Captions)
			}
			w.open, w.closingAt = true, time.Time{}
		} else if w.open {
			if w.closingAt.IsZero() {
				w.closingAt = now
			}
			if now.Sub(w.closingAt) >= w.timing.CloseDelay {
				w.open, w.yielded, w.closingAt, w.closedAt = false, false, time.Time{}, now
				w.log("info", "%s: closed in FL Studio", w.Link.Name)
				w.retryAt, w.pollNow = time.Time{}, true
			}
		}
	}

	// 3. The server.
	if w.pollNow || !w.known || now.Sub(w.polledAt) >= w.timing.Poll {
		w.poll(now)
	}
	if !w.known || !w.online || w.gone || w.authFailed {
		return
	}
	shared := w.project.MemberCount > 1
	other := w.otherHolder() != ""
	atHead := w.project.Head == w.eng.BaseID()
	canRetry := !now.Before(w.retryAt)

	// 4a. Open in FL Studio: hold the lock, upload every save, never pull.
	if w.open {
		if shared && !w.holding && !w.yielded && !other && atHead && !now.Before(w.lockRetryAt) {
			w.acquire(now)
		}
		w.beat(now)
		if w.dirty() && quiet && atHead && (!shared || w.holding) && canRetry && w.needsConfirm == "" && (w.autoSend || w.sendOnce) {
			w.sendOnce = false
			w.run(now, "Uploading your save", func() (engine.Result, error) { return w.eng.Push(w.pushOptions()) })
		}
		return
	}

	// 4b. Not open: pull what is new, upload what changed here.
	w.beat(now)
	wantPush := w.dirty() && quiet && w.needsConfirm == "" && (!shared || w.holding || !other) && (w.autoSend || w.sendOnce)
	wantPull := !atHead && w.project.Head != "" && (w.autoUpdate || w.updateOnce)
	fullCheck := w.synced && quiet && now.Sub(w.lastFull) >= w.timing.FullCheck
	if canRetry && !view.Saving {
		switch {
		case wantPush || fullCheck:
			w.sendOnce = false
			w.run(now, "Syncing", func() (engine.Result, error) { return w.eng.Sync(w.pushOptions()) })
		case wantPull:
			w.updateOnce = false
			w.run(now, "Downloading the newest version", func() (engine.Result, error) { return w.eng.Pull() })
		}
	}
	if w.holding {
		// The session is over. Give the lock back once the last save is on
		// the server, or after a while even if that keeps failing: the save
		// stays on this computer and is handled like any other local change.
		if !w.dirty() || now.Sub(w.closedAt) >= w.timing.HoldAfterClose {
			w.release()
		}
	}
	if w.synced && !w.dirty() {
		w.eng.CleanTrash(w.timing.TrashAge)
	}
}

// poll asks the server for the project's head, lock and members.
func (w *Worker) poll(now time.Time) {
	w.polledAt, w.pollNow = now, false
	view, err := w.api.View()
	defer func() {
		if w.online != w.wasOnline || !w.pollLogged {
			if w.online {
				w.log("info", "%s: server reachable", w.Link.Name)
			} else {
				w.log("error", "%s: server not reachable: %v", w.Link.Name, err)
			}
		}
		w.wasOnline, w.pollLogged = w.online, true
	}()
	switch {
	case err == nil:
	case errors.Is(err, core.ErrNotFound):
		w.online, w.gone = true, true
		return
	case errors.Is(err, core.ErrSignIn):
		w.online, w.authFailed = true, true
		return
	default:
		w.online = false
		return
	}
	w.online, w.gone, w.authFailed, w.known = true, false, false, true
	w.project = view
	mine := view.Lock != nil && view.Lock.User.ID == w.me.ID && view.Lock.Device == w.device
	switch {
	case view.MemberCount <= 1:
		w.holding = false
	case w.holding && !mine:
		w.holding = false // taken over after a long silence, or released by the owner
	case !w.holding && mine:
		w.holding, w.heartbeatAt = true, now // the app restarted while it held the lock
	}
}

func (w *Worker) acquire(now time.Time) {
	err := w.api.AcquireLock(w.eng.BaseID())
	switch {
	case err == nil:
		w.holding, w.heartbeatAt = true, now
		w.project.Lock = &core.LockView{User: w.me, Device: w.device}
		w.log("info", "%s: took the lock", w.Link.Name)
	case errors.Is(err, core.ErrLockHeld), errors.Is(err, core.ErrNotAtHead):
		w.pollNow, w.lockRetryAt = true, now.Add(w.timing.Quiet)
		w.log("debug", "%s: could not take the lock: %v", w.Link.Name, err)
	default:
		w.pollNow, w.lockRetryAt = true, now.Add(w.timing.RetryMin)
		w.log("error", "%s: could not take the lock: %v", w.Link.Name, err)
	}
}

// beat renews a held lock.
func (w *Worker) beat(now time.Time) {
	if !w.holding || now.Sub(w.heartbeatAt) < w.timing.Heartbeat {
		return
	}
	switch err := w.api.Heartbeat(); {
	case err == nil:
		w.heartbeatAt = now
	case errors.Is(err, core.ErrLockLost):
		w.holding, w.pollNow = false, true
		w.log("error", "%s: the lock was lost", w.Link.Name)
	default:
		w.heartbeatAt = now.Add(w.timing.RetryMin - w.timing.Heartbeat) // try again soon
	}
}

func (w *Worker) release() {
	if err := w.api.ReleaseLock(); err != nil {
		w.pollNow = true // the server decides who holds it; ask again
	}
	w.holding = false
	if w.project.Lock != nil && w.project.Lock.User.ID == w.me.ID && w.project.Lock.Device == w.device {
		w.project.Lock = nil
	}
	w.log("info", "%s: gave the lock back", w.Link.Name)
}

// run performs one engine operation and digests its outcome.
func (w *Worker) run(now time.Time, note string, op func() (engine.Result, error)) (engine.Result, error) {
	before := w.view.Sum
	w.setBusy(note)
	started := time.Now()
	res, err := op()
	w.setBusy("")
	w.log("debug", "%s: %s took %s: uploaded %d, downloaded %d files, err=%v", w.Link.Name, note,
		time.Since(started).Round(time.Millisecond), res.Uploaded, res.Downloaded, err)

	if res.Pulled || len(res.ConflictCopies) > 0 {
		// The engine changed files itself. Look again so its own writes are
		// not mistaken for new edits.
		w.view = viewFolder(w.Link.Folder)
		w.synced = err == nil
		w.syncedSum = w.view.Sum
		w.refreshFLPInfo()
	} else if err == nil {
		w.synced, w.syncedSum = true, before
	}
	if res.Pushed || res.Pulled {
		w.project.Head = w.eng.BaseID()
		w.lastSync = now
		w.lastActivity = describeResult(res)
		w.log("info", "%s: %s", w.Link.Name, w.lastActivity)
	}

	switch engine.KindOf(err) {
	case engine.KindNone:
		w.failures, w.problem, w.retryAt, w.lastFull = 0, "", time.Time{}, now
		if res.Pushed {
			w.allowMassDelete, w.nextNote = false, "" // both were for that one upload
		}
	case engine.KindBusy:
		w.retryAt = now.Add(w.timing.Quiet)
		w.log("debug", "%s: %s postponed: %v", w.Link.Name, note, err)
	case engine.KindLockNotHeld:
		w.holding, w.pollNow = false, true
		w.retryAt = now.Add(w.timing.RetryMin)
		w.log("info", "%s: not uploaded, a partner holds the project", w.Link.Name)
	case engine.KindHeadMoved:
		w.pollNow = true
		w.retryAt = now.Add(w.timing.RetryMin)
		w.log("info", "%s: a newer version arrived first; trying again", w.Link.Name)
	case engine.KindMassDelete:
		w.needsConfirm = err.Error()
		w.log("error", "%s: upload stopped, waiting for your yes: %v", w.Link.Name, err)
	case engine.KindFolderMissing:
	default:
		w.failures++
		w.problem = err.Error()
		wait := w.timing.RetryMin << min(w.failures-1, 6)
		if wait > w.timing.RetryMax || wait <= 0 {
			wait = w.timing.RetryMax
		}
		w.retryAt, w.pollNow = now.Add(wait), true
		w.log("error", "%s: %v (next try in %s)", w.Link.Name, err, wait)
	}
	return res, err
}

func describeResult(res engine.Result) string {
	var parts []string
	if res.Pulled {
		parts = append(parts, "downloaded the newest version")
	}
	if res.Pushed {
		parts = append(parts, "uploaded your changes")
	}
	if n := len(res.ConflictCopies); n == 1 {
		parts = append(parts, "kept your version as "+res.ConflictCopies[0])
	} else if n > 1 {
		parts = append(parts, fmt.Sprintf("kept %d of your files as conflict copies", n))
	}
	text := strings.Join(parts, ", ")
	if text == "" {
		return ""
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// --- project files ---------------------------------------------------------

// refreshFLPInfo reads the project files that changed since they were last
// read. Only called when the folder is quiet, so a file is never read mid-save.
func (w *Worker) refreshFLPInfo() {
	for rel, st := range w.view.FLPs {
		if entry, ok := w.flps[rel]; ok && entry.st == st {
			continue
		}
		info, err := flp.ReadFile(filepath.Join(w.Link.Folder, filepath.FromSlash(rel)))
		w.flps[rel] = flpEntry{st: st, info: info, ok: err == nil}
	}
	for rel := range w.flps {
		if _, ok := w.view.FLPs[rel]; !ok {
			delete(w.flps, rel)
		}
	}
}

// flpNames lists the file names and the stored titles of the project files.
func (w *Worker) flpNames() (names, titles []string) {
	for rel := range w.view.FLPs {
		names = append(names, rel[strings.LastIndexByte(rel, '/')+1:])
	}
	for _, entry := range w.flps {
		if entry.ok && entry.info.Title != "" {
			titles = append(titles, entry.info.Title)
		}
	}
	return names, titles
}

// pushOptions describes the next upload: which FL Studio version and plugins
// the most recently saved project file uses.
func (w *Worker) pushOptions() engine.PushOptions {
	opts := engine.PushOptions{AllowMassDelete: w.allowMassDelete, Message: w.nextNote}
	var newest int64
	for _, entry := range w.flps {
		if entry.ok && entry.st.MTime >= newest {
			newest = entry.st.MTime
			opts.FLVersion = entry.info.FLVersion
			opts.Plugins = entry.info.PluginNames()
		}
	}
	if len(opts.Plugins) > 200 {
		opts.Plugins = opts.Plugins[:200]
	}
	return opts
}

// baseName returns the last element of a path written with either separator.
func baseName(path string) string {
	return path[strings.LastIndexAny(path, `\/`)+1:]
}

// outsideSample is a sample a project file refers to that lives outside the
// project folder, so a partner would not get it.
type outsideSample struct {
	Path   string // as stored in the project file
	FLPDir string // folder of the project file, on this computer
	Source string // where it is on this computer: Path, or found by name; "" if nowhere
}

func (w *Worker) outsideSamples() []outsideSample {
	var out []outsideSample
	seen := map[string]bool{}
	rels := make([]string, 0, len(w.flps))
	for rel := range w.flps {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		entry := w.flps[rel]
		if !entry.ok {
			continue
		}
		dir := filepath.Dir(filepath.Join(w.Link.Folder, filepath.FromSlash(rel)))
		for _, sample := range entry.info.SamplePaths {
			if flp.ClassifySample(sample, w.Link.Folder) != flp.Outside || seen[strings.ToLower(sample)] {
				continue
			}
			// FL Studio looks for a missing sample next to the project
			// file, so a copy there is as good as the original, unless
			// the file there is a different sample with the same name.
			source := w.finder.find(sample)
			if there, err := os.Stat(filepath.Join(dir, baseName(sample))); err == nil {
				if orig, err := os.Stat(source); source == "" || err != nil || orig.Size() == there.Size() {
					continue
				}
			}
			seen[strings.ToLower(sample)] = true
			out = append(out, outsideSample{Path: sample, FLPDir: dir, Source: source})
		}
	}
	return out
}

// --- what the user sees ----------------------------------------------------

func (w *Worker) publish() {
	st := Status{
		ProjectID: w.Link.ProjectID, Name: w.Link.Name, Folder: w.Link.Folder,
		Online: w.online, OpenInFL: w.open, HoldingLock: w.holding, Yielded: w.yielded,
		Conflicts: append([]string{}, w.view.Conflicts...), OutsideSamples: []string{}, MissingSamples: []string{}, Members: []string{},
		ConfirmDelete: w.needsConfirm, LastActivity: w.lastActivity, NextNote: w.nextNote, Gone: w.gone,
		AutoSend: w.autoSend, AutoUpdate: w.autoUpdate,
	}
	if !w.lastSync.IsZero() {
		st.LastSync = w.lastSync.UTC().Format(time.RFC3339)
	}
	if w.known {
		st.Name = w.project.Name
		st.Shared = w.project.MemberCount > 1
		st.Role = w.project.Role
		st.RetentionDays = w.project.RetentionDays
		st.LockHolder = w.otherHolder()
		st.NewerOnServer = w.project.Head != "" && w.project.Head != w.eng.BaseID()
		for _, m := range w.project.Members {
			st.Members = append(st.Members, m.User.Name)
		}
		if w.holding && w.project.Lock != nil {
			st.LockRequestedBy = w.project.Lock.RequestedBy
		}
	}
	st.UnsentChanges = w.seen && !w.view.Missing && w.dirty()
	for _, sample := range w.outsideSamples() {
		if sample.Source != "" {
			st.OutsideSamples = append(st.OutsideSamples, sample.Path)
		} else {
			st.MissingSamples = append(st.MissingSamples, baseName(sample.Path))
		}
	}
	st.State, st.Headline, st.Detail = w.sentence(st)
	if st.Headline != w.lastHeadline {
		level := "info"
		if st.State == "problem" {
			level = "error"
		}
		w.log(level, "%s", strings.TrimSpace(st.Name+": "+st.Headline+". "+st.Detail))
		w.lastHeadline = st.Headline
	}

	w.statusMu.Lock()
	w.status = st
	w.statusMu.Unlock()
}

// sentence turns the worker's situation into one word, a headline and a
// sentence or two.
func (w *Worker) sentence(st Status) (state, headline, detail string) {
	unsent := ""
	if st.UnsentChanges {
		unsent = " Your unsent saves are safe on this computer."
	}
	switch {
	case w.view.Missing:
		return "problem", "Folder not found", "The project folder is missing or was moved. Nothing was deleted on the server. Put the folder back, or unlink the project and link it to its new place."
	case w.authFailed:
		return "problem", "Sign-in problem", "The server did not accept this computer's token. Sign in again."
	case w.gone:
		return "problem", "No longer available", "This project was removed from the server, or you were removed from it. Your files stay on this computer."
	case !w.online:
		return "offline", "Offline", "The server cannot be reached. Your saves stay on this computer and are uploaded when it is back."
	case !w.known:
		return "syncing", "Starting", ""
	case w.needsConfirm != "":
		return "attention", "Check before uploading", "More than half of this project's files were removed from the folder (" + w.needsConfirm + "). If that is what you want, confirm it and the removal is uploaded. If not, put the files back."
	case w.problem != "":
		return "problem", "Something went wrong", w.problem + ". Trying again shortly."
	}

	if w.open {
		switch {
		case st.NewerOnServer && st.LockHolder != "":
			return "waiting", st.LockHolder + " is editing", "You are looking at an older version. Close the project in FL Studio when " + st.LockHolder + " is done and the newest version is loaded." + keepCopy(st)
		case st.NewerOnServer:
			return "attention", "A newer version is waiting", "Close the project in FL Studio to load it." + keepCopy(st)
		case st.Shared && st.LockHolder != "":
			return "waiting", st.LockHolder + " is editing", "You can look around, but your saves are not uploaded until " + st.LockHolder + " is done." + unsent
		case st.Shared && w.yielded:
			return "waiting", "You handed the project over", "Your saves are not uploaded for now. Take the project back when your partner is done." + unsent
		case st.Shared && !w.holding:
			return "syncing", "Open in FL Studio", "Getting the project ready for editing."
		case st.LockRequestedBy != "":
			return "editing", "You are editing", st.LockRequestedBy + " would like to edit. Every save is uploaded; hand the project over when you are done."
		case st.UnsentChanges:
			return "editing", "You are editing", "Uploading your latest save."
		case st.Shared:
			return "editing", "You are editing", "Every save is uploaded. Your partner sees the project as locked until you close it."
		default:
			return "editing", "Open in FL Studio", "Every save is uploaded."
		}
	}
	switch {
	case st.UnsentChanges && st.Shared && st.LockHolder != "" && !w.holding:
		return "waiting", "Waiting for " + st.LockHolder, st.LockHolder + " is editing. Your changes are uploaded when they are done."
	case st.UnsentChanges || st.NewerOnServer:
		return "syncing", "Syncing", ""
	case st.LockHolder != "":
		return "synced", "Up to date", st.LockHolder + " is editing right now."
	default:
		return "synced", "Up to date", ""
	}
}

func keepCopy(st Status) string {
	if st.UnsentChanges {
		return " Your unsent saves will be kept as a separate copy next to it."
	}
	return ""
}

// --- the user's actions ----------------------------------------------------

// SyncNow forgets any waiting period so the next Tick tries right away.
func (w *Worker) SyncNow() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.retryAt, w.lockRetryAt, w.lastFull, w.pollNow = time.Time{}, time.Time{}, time.Time{}, true
	w.synced = false
}

// SendNow triggers one upload even when auto-send is off.
func (w *Worker) SendNow() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sendOnce = true
	w.retryAt, w.lastFull, w.pollNow = time.Time{}, time.Time{}, true
	w.synced = false
}

// UpdateNow triggers one download even when auto-update is off.
func (w *Worker) UpdateNow() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.updateOnce = true
	w.retryAt, w.pollNow = time.Time{}, true
}

// SetAutoSend turns automatic uploading on or off for this project.
func (w *Worker) SetAutoSend(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.autoSend = on
	if on {
		w.retryAt, w.lastFull = time.Time{}, time.Time{}
		w.synced = false
	}
	w.publish()
}

// SetAutoUpdate turns automatic downloading on or off for this project.
func (w *Worker) SetAutoUpdate(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.autoUpdate = on
	if on {
		w.retryAt, w.pollNow = time.Time{}, true
	}
	w.publish()
}

// MainFLP returns the path of the main .flp file in the project folder:
// the most recently modified one.
func (w *Worker) MainFLP() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var best string
	var bestTime int64
	for rel, entry := range w.flps {
		if entry.st.MTime >= bestTime {
			bestTime = entry.st.MTime
			best = rel
		}
	}
	if best == "" {
		// No parsed FLP; fall back to any .flp in the view.
		for rel, st := range w.view.FLPs {
			if st.MTime >= bestTime {
				bestTime = st.MTime
				best = rel
			}
		}
	}
	if best == "" {
		return ""
	}
	return filepath.Join(w.Link.Folder, filepath.FromSlash(best))
}

// ConfirmDelete lets the next upload remove more than half of the files.
func (w *Worker) ConfirmDelete() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.allowMassDelete, w.needsConfirm, w.retryAt = true, "", time.Time{}
}

// Yield hands the lock over to a partner while the project stays open here.
// The last save is uploaded first.
func (w *Worker) Yield() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer w.publish()
	if !w.holding {
		return errors.New("this computer does not hold the project right now")
	}
	w.view = viewFolder(w.Link.Folder)
	if w.view.Saving {
		return errors.New("FL Studio is saving; try again in a moment")
	}
	if w.dirty() {
		if _, err := w.run(w.now(), "Uploading your save", func() (engine.Result, error) { return w.eng.Push(w.pushOptions()) }); err != nil {
			return fmt.Errorf("the last save could not be uploaded, so the project was not handed over: %w", err)
		}
	}
	w.release()
	w.yielded, w.pollNow = true, true
	return nil
}

// TakeLock undoes Yield: the project is taken for editing again as soon as
// it is free.
func (w *Worker) TakeLock() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.yielded, w.lockRetryAt, w.pollNow = false, time.Time{}, true
}

// AskForLock tells the member who is editing that this user would like to.
func (w *Worker) AskForLock() error { return w.api.RequestLock() }

// BreakLock frees a lock someone else holds. Only the owner may do this; it
// is meant for a partner whose computer went away while holding the project.
func (w *Worker) BreakLock() error {
	err := w.api.ForceRelease()
	w.mu.Lock()
	w.pollNow = true
	w.mu.Unlock()
	return err
}

// SetNote sets what the history will say about the next save, like a commit
// message. Without a note the app describes the change itself.
func (w *Worker) SetNote(text string) error {
	text = strings.TrimSpace(text)
	if len(text) > 300 {
		return errors.New("a note is at most 300 characters")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextNote = text
	w.publish()
	return nil
}

// Comments lists the comments on the project's versions, oldest first.
func (w *Worker) Comments() ([]core.Comment, error) { return w.api.Comments() }

// Comment leaves a remark on one version.
func (w *Worker) Comment(snapshotID, text string) error {
	_, err := w.api.AddComment(snapshotID, text)
	if err != nil && errors.Is(err, core.ErrNotFound) {
		return errors.New("that version no longer exists")
	}
	return err
}

// History lists the project's versions, newest first.
func (w *Worker) History() ([]core.Snapshot, error) { return w.api.History() }

// Restore brings an earlier version back as the newest one.
func (w *Worker) Restore(snapshotID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer w.publish()
	if w.open {
		return errors.New("close the project in FL Studio first, then restore")
	}
	w.poll(w.now()) // what was seen seconds ago may be a lock held just for one upload
	if holder := w.otherHolder(); holder != "" {
		return fmt.Errorf("%s is editing; restore when they are done", holder)
	}
	w.view = viewFolder(w.Link.Folder)
	_, err := w.run(w.now(), "Restoring", func() (engine.Result, error) { return w.eng.Restore(snapshotID) })
	return err
}

// CollectResult says what CollectSamples did.
type CollectResult struct {
	Copied   []string `json:"copied"`   // file names now in the project folder
	NotFound []string `json:"notFound"` // samples that are not on this computer either
	Clashes  []string `json:"clashes"`  // a different file with the same name is already there
}

// CollectSamples copies the samples a project file uses from elsewhere on
// this computer into the project folder, next to the project file. FL Studio
// looks there when a sample is not at its original place, so partners get
// the samples with the project.
func (w *Worker) CollectSamples() (CollectResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer w.publish()
	w.view = viewFolder(w.Link.Folder)
	if w.view.Missing {
		return CollectResult{}, errors.New("the project folder is missing")
	}
	if w.view.Saving {
		return CollectResult{}, errors.New("FL Studio is saving; try again in a moment")
	}
	w.refreshFLPInfo()
	return w.collect()
}

// collect copies every outside sample that is on this computer into the
// project folder. The caller holds w.mu and has made sure FL Studio is not
// in the middle of a save.
func (w *Worker) collect() (CollectResult, error) {
	var result CollectResult
	for _, sample := range w.outsideSamples() {
		name := baseName(sample.Path)
		target := filepath.Join(sample.FLPDir, name)
		source, err := os.Stat(sample.Source)
		if err != nil || source.IsDir() {
			result.NotFound = append(result.NotFound, sample.Path)
			continue
		}
		if _, err := os.Lstat(target); err == nil || core.ValidatePath(name) != nil {
			// Two samples with the same file name, or a name that cannot be
			// synced: FL Studio could not tell them apart in one folder.
			result.Clashes = append(result.Clashes, sample.Path)
			continue
		}
		if err := copyInto(sample.Source, target); err != nil {
			return result, fmt.Errorf("copying %s: %w", name, err)
		}
		result.Copied = append(result.Copied, name)
	}
	return result, nil
}

// copyInto copies a file to a path that does not exist yet.
func copyInto(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := to + ".part" // ignored by the sync until it is complete
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if _, err := os.Lstat(to); err == nil {
		os.Remove(tmp)
		return errors.New("a file with that name appeared in the meantime")
	}
	return os.Rename(tmp, to)
}

// Stop ends the worker's session: a held lock is given back.
func (w *Worker) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.holding {
		w.release()
	}
}
