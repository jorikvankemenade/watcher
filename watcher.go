package watcher

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"strconv"

	"github.com/go-logr/logr"
)

var (
	// ErrDurationTooShort occurs when calling the watcher's Start
	// method with a duration that's less than 1 nanosecond.
	ErrDurationTooShort = errors.New("error: duration is less than 1ns")

	// ErrWatcherRunning occurs when trying to call the watcher's
	// Start method and the polling cycle is still already running
	// from previously calling Start and not yet calling Close.
	ErrWatcherRunning = errors.New("error: watcher is already running")

	// ErrWatchedFileDeleted is an error that occurs when a file or folder that was
	// being watched has been deleted.
	ErrWatchedFileDeleted = errors.New("error: watched file or folder deleted")

	// ErrSkip is less of an error, but more of a way for path hooks to skip a file or
	// directory.
	ErrSkip = errors.New("error: skipping file")
)

// An Op is a type that is used to describe what type
// of event has occurred during the watching process.
type Op uint32

// Ops
const (
	Create Op = iota
	Write
	Remove
	Rename
	Chmod
	Move
)

var ops = map[Op]string{
	Create: "CREATE",
	Write:  "WRITE",
	Remove: "REMOVE",
	Rename: "RENAME",
	Chmod:  "CHMOD",
	Move:   "MOVE",
}

// String prints the string version of the Op consts
func (e Op) String() string {
	if op, found := ops[e]; found {
		return op
	}
	return "???"
}

// An Event describes an event that is received when files or directory
// changes occur. It includes the os.FileInfo of the changed file or
// directory and the type of event that's occurred and the full path of the file.
type Event struct {
	Op
	Path    string
	OldPath string
	os.FileInfo
}

// String returns a string depending on what type of event occurred and the
// file name associated with the event.
func (e Event) String() string {
	if e.FileInfo == nil {
		return "???"
	}

	pathType := "FILE"
	if e.IsDir() {
		pathType = "DIRECTORY"
	}
	return fmt.Sprintf("%s %q %s [%s]", pathType, e.Name(), e.Op, e.Path)
}

// FilterFileHookFunc is a function that is called to filter files during listings.
// If a file is ok to be listed, nil is returned otherwise ErrSkip is returned.
type FilterFileHookFunc func(info os.FileInfo, fullPath string) error

// RegexFilterHook is a function that accepts or rejects a file
// for listing based on whether it's filename or full path matches
// a regular expression.
func RegexFilterHook(r *regexp.Regexp, useFullPath bool) FilterFileHookFunc {
	return func(info os.FileInfo, fullPath string) error {
		str := info.Name()

		if useFullPath {
			str = fullPath
		}

		// Match
		if r.MatchString(str) {
			return nil
		}

		// No match.
		return ErrSkip
	}
}

// Watcher describes a process that watches files for changes.
type Watcher struct {
	Event  chan Event
	Error  chan error
	Closed chan struct{}
	close  chan struct{}
	wg     *sync.WaitGroup
	logger logr.Logger

	// mu protects the following.
	mu           *sync.Mutex
	ffh          []FilterFileHookFunc
	running      bool
	names        map[string]bool        // bool for recursive or not.
	files        map[string]os.FileInfo // map of files.
	ignored      map[string]struct{}    // ignored files or directories.
	ops          map[Op]struct{}        // Op filtering.
	ignoreHidden bool                   // ignore hidden files or not.
	maxEvents    int                    // max sent events per cycle
}

// New creates a new Watcher.
func New(logger logr.Logger) *Watcher {
	// Set up the WaitGroup for w.Wait().
	var wg sync.WaitGroup
	wg.Add(1)

	return &Watcher{
		Event:   make(chan Event),
		Error:   make(chan error),
		Closed:  make(chan struct{}),
		close:   make(chan struct{}),
		mu:      new(sync.Mutex),
		wg:      &wg,
		logger:  logger,
		files:   make(map[string]os.FileInfo),
		ignored: make(map[string]struct{}),
		names:   make(map[string]bool),
	}
}

// SetMaxEvents controls the maximum amount of events that are sent on
// the Event channel per watching cycle. If max events is less than 1, there is
// no limit, which is the default.
func (w *Watcher) SetMaxEvents(delta int) {
	w.mu.Lock()
	w.maxEvents = delta
	w.mu.Unlock()
}

// AddFilterHook
func (w *Watcher) AddFilterHook(f FilterFileHookFunc) {
	w.mu.Lock()
	w.ffh = append(w.ffh, f)
	w.mu.Unlock()
}

// IgnoreHiddenFiles sets the watcher to ignore any file or directory
// that starts with a dot.
func (w *Watcher) IgnoreHiddenFiles(ignore bool) {
	w.mu.Lock()
	w.ignoreHidden = ignore
	w.mu.Unlock()
}

// FilterOps filters which event op types should be returned
// when an event occurs.
func (w *Watcher) FilterOps(ops ...Op) {
	w.mu.Lock()
	w.ops = make(map[Op]struct{})
	for _, op := range ops {
		w.ops[op] = struct{}{}
	}
	w.mu.Unlock()
}

// Add adds either a single file or directory to the file list.
func (w *Watcher) Add(name string) (err error) {
	w.logger.Info("Adding " + name + " to the file list")

	w.mu.Lock()
	defer w.mu.Unlock()

	name, err = filepath.Abs(name)
	if err != nil {
		return err
	}

	// If name is on the ignored list or if hidden files are
	// ignored and name is a hidden file or directory, simply return.
	_, ignored := w.ignored[name]

	isHidden, err := isHiddenFile(name)
	if err != nil {
		return err
	}

	if ignored || (w.ignoreHidden && isHidden) {
		return nil
	}

	// Add the directory's contents to the files list.
	fileList, err := w.list(name)
	if err != nil {
		return err
	}
	for k, v := range fileList {
		w.files[k] = v
	}

	// Add the name to the names list.
	w.names[name] = false

	return nil
}

func (w *Watcher) list(name string) (map[string]os.FileInfo, error) {
	fileList := make(map[string]os.FileInfo)

	// Make sure name exists.
	stat, err := os.Stat(name)
	if err != nil {
		return nil, err
	}

	fileList[name] = stat

	// If it's not a directory, just return.
	if !stat.IsDir() {
		return fileList, nil
	}

	// It's a directory.
	fInfoList, err := ioutil.ReadDir(name)
	if err != nil {
		return nil, err
	}
	// Add all of the files in the directory to the file list as long
	// as they aren't on the ignored list or are hidden files if ignoreHidden
	// is set to true.
outer:
	for _, fInfo := range fInfoList {
		path := filepath.Join(name, fInfo.Name())
		_, ignored := w.ignored[path]

		isHidden, err := isHiddenFile(path)
		if err != nil {
			return nil, err
		}

		if ignored || (w.ignoreHidden && isHidden) {
			continue
		}

		for _, f := range w.ffh {
			err := f(fInfo, path)
			if err == ErrSkip {
				continue outer
			}
			if err != nil {
				return nil, err
			}
		}

		fileList[path] = fInfo
	}
	return fileList, nil
}

// AddRecursive adds either a single file or directory recursively to the file list.
func (w *Watcher) AddRecursive(name string) (err error) {
	w.logger.Info("Recursively adding " + name)
	w.mu.Lock()
	defer w.mu.Unlock()

	name, err = filepath.Abs(name)
	if err != nil {
		return err
	}

	fileList, err := w.listRecursive(name)
	if err != nil {
		return err
	}
	for k, v := range fileList {
		w.logger.Info("Adding " + v.Name() + " to the file list")
		w.files[k] = v
	}

	// Add the name to the names list.
	w.names[name] = true

	return nil
}

func (w *Watcher) listRecursive(name string) (map[string]os.FileInfo, error) {
	fileList := make(map[string]os.FileInfo)

	return fileList, filepath.Walk(name, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		for _, f := range w.ffh {
			err := f(info, path)
			if err == ErrSkip {
				return nil
			}
			if err != nil {
				return err
			}
		}

		// If path is ignored and it's a directory, skip the directory. If it's
		// ignored and it's a single file, skip the file.
		_, ignored := w.ignored[path]

		isHidden, err := isHiddenFile(path)
		if err != nil {
			return err
		}

		if ignored || (w.ignoreHidden && isHidden) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Add the path and it's info to the file list.
		fileList[path] = info
		return nil
	})
}

// Remove removes either a single file or directory from the file's list.
func (w *Watcher) Remove(name string) (err error) {
	w.logger.Info("Remove " + name + " from the file list")

	w.mu.Lock()
	defer w.mu.Unlock()

	name, err = filepath.Abs(name)
	if err != nil {
		return err
	}

	// Remove the name from w's names list.
	delete(w.names, name)

	// If name is a single file, remove it and return.
	info, found := w.files[name]
	if !found {
		return nil // Doesn't exist, just return.
	}
	if !info.IsDir() {
		delete(w.files, name)
		return nil
	}

	// Delete the actual directory from w.files
	delete(w.files, name)

	// If it's a directory, delete all of it's contents from w.files.
	for path := range w.files {
		if filepath.Dir(path) == name {
			delete(w.files, path)
		}
	}
	return nil
}

// RemoveRecursive removes either a single file or a directory recursively from
// the file's list.
func (w *Watcher) RemoveRecursive(name string) (err error) {
	w.logger.Info("Recursively removing " + name)
	w.mu.Lock()
	defer w.mu.Unlock()

	name, err = filepath.Abs(name)
	if err != nil {
		return err
	}

	// Remove the name from w's names list.
	delete(w.names, name)

	// If name is a single file, remove it and return.
	info, found := w.files[name]
	if !found {
		return nil // Doesn't exist, just return.
	}
	if !info.IsDir() {
		delete(w.files, name)
		return nil
	}

	// If it's a directory, delete all of it's contents recursively
	// from w.files.
	for path := range w.files {
		if strings.HasPrefix(path, name) {
			delete(w.files, path)
		}
	}
	return nil
}

// Ignore adds paths that should be ignored.
//
// For files that are already added, Ignore removes them.
func (w *Watcher) Ignore(paths ...string) (err error) {
	for _, path := range paths {
		path, err = filepath.Abs(path)
		if err != nil {
			return err
		}
		// Remove any of the paths that were already added.
		if err := w.RemoveRecursive(path); err != nil {
			return err
		}
		w.mu.Lock()
		w.ignored[path] = struct{}{}
		w.mu.Unlock()
	}
	return nil
}

// WatchedFiles returns a map of files added to a Watcher.
func (w *Watcher) WatchedFiles() map[string]os.FileInfo {
	w.mu.Lock()
	defer w.mu.Unlock()

	files := make(map[string]os.FileInfo)
	for k, v := range w.files {
		files[k] = v
	}

	return files
}

// fileInfo is an implementation of os.FileInfo that can be used
// as a mocked os.FileInfo when triggering an event when the specified
// os.FileInfo is nil.
type fileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	sys     interface{}
	dir     bool
}

func (fs *fileInfo) IsDir() bool {
	return fs.dir
}
func (fs *fileInfo) ModTime() time.Time {
	return fs.modTime
}
func (fs *fileInfo) Mode() os.FileMode {
	return fs.mode
}
func (fs *fileInfo) Name() string {
	return fs.name
}
func (fs *fileInfo) Size() int64 {
	return fs.size
}
func (fs *fileInfo) Sys() interface{} {
	return fs.sys
}

// TriggerEvent is a method that can be used to trigger an event, separate to
// the file watching process.
func (w *Watcher) TriggerEvent(eventType Op, file os.FileInfo) {
	w.Wait()
	if file == nil {
		file = &fileInfo{name: "triggered event", modTime: time.Now()}
	}
	w.Event <- Event{Op: eventType, Path: "-", FileInfo: file}
}

func (w *Watcher) retrieveFileList() map[string]os.FileInfo {
	w.mu.Lock()
	defer w.mu.Unlock()

	fileList := make(map[string]os.FileInfo)

	var list map[string]os.FileInfo
	var err error

	for name, recursive := range w.names {
		if recursive {
			list, err = w.listRecursive(name)
			if err != nil {
				if os.IsNotExist(err) {
					w.mu.Unlock()
					if name == err.(*os.PathError).Path {
						w.Error <- ErrWatchedFileDeleted
						w.RemoveRecursive(name)
					}
					w.mu.Lock()
				} else {
					w.Error <- err
				}
			}
		} else {
			list, err = w.list(name)
			if err != nil {
				if os.IsNotExist(err) {
					w.mu.Unlock()
					if name == err.(*os.PathError).Path {
						w.Error <- ErrWatchedFileDeleted
						w.Remove(name)
					}
					w.mu.Lock()
				} else {
					w.Error <- err
				}
			}
		}
		// Add the file's to the file list.
		for k, v := range list {
			fileList[k] = v
		}
	}

	return fileList
}

// Start begins the polling cycle which repeats every specified
// duration until Close is called.
func (w *Watcher) Start(d time.Duration) error {
	w.logger.Info("Start watcher")

	// Return an error if d is less than 1 nanosecond.
	if d < time.Nanosecond {
		return ErrDurationTooShort
	}

	// Make sure the Watcher is not already running.
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return ErrWatcherRunning
	}
	w.running = true
	w.mu.Unlock()

	// Unblock w.Wait().
	w.wg.Done()

	for {
		// done lets the inner polling cycle loop know when the
		// current cycle's method has finished executing.
		done := make(chan struct{})

		// Any events that are found are first piped to evt before
		// being sent to the main Event channel.
		evt := make(chan Event)

		// Retrieve the file list for all watched file's and dirs.
		fileList := w.retrieveFileList()

		// cancel can be used to cancel the current event polling function.
		cancel := make(chan struct{})

		// Look for events.
		go func() {
			w.pollEvents(fileList, evt, cancel)
			done <- struct{}{}
		}()

		// numEvents holds the number of events for the current cycle.
		numEvents := 0

	inner:
		for {
			select {
			case <-w.close:
				close(cancel)
				close(w.Closed)
				return nil
			case event := <-evt:
				if len(w.ops) > 0 { // Filter Ops.
					_, found := w.ops[event.Op]
					if !found {
						continue
					}
				}
				numEvents++
				if w.maxEvents > 0 && numEvents > w.maxEvents {
					close(cancel)
					break inner
				}
				w.Event <- event
			case <-done: // Current cycle is finished.
				break inner
			}
		}

		// Update the file's list.
		w.mu.Lock()
		w.files = fileList
		w.mu.Unlock()

		// Sleep and then continue to the next loop iteration.
		time.Sleep(d)
	}
}

func (w *Watcher) pollEvents(files map[string]os.FileInfo, evt chan Event,
	cancel chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Store create and remove events for use to check for rename events.
	creates := make(map[string]os.FileInfo)
	removes := make(map[string]os.FileInfo)

	// Check for removed files.
	for path, info := range w.files {
		if _, found := files[path]; !found {
			w.logger.Info("Removed file: " + path)
			removes[path] = info
		}
	}

	// Check for created files, writes and chmods.
	for path, info := range files {
		oldInfo, found := w.files[path]
		if !found {
			w.logger.Info("Created file: " + path)
			// A file was created.
			creates[path] = info
			continue
		}
		if oldInfo.ModTime() != info.ModTime() {
			select {
			case <-cancel:
				return
			case evt <- Event{Write, path, path, info}:
			}
		}
		if oldInfo.Mode() != info.Mode() {
			select {
			case <-cancel:
				return
			case evt <- Event{Chmod, path, path, info}:
			}
		}
	}

	if len(removes) != 0 {
		w.logger.Info("Remove actions: ")
		for file_path, info := range removes {
			w.logger.Info(info.Name() + " (" + file_path + ")")
		}
	}

	if len(creates) != 0 {
		w.logger.Info("Create actions: ")
		for file_path, info := range creates {
			w.logger.Info(info.Name() + " (" + file_path + ")")
		}
	}

	// Check for renames and moves.
	for path1, info1 := range removes {
		w.logger.Info("Check remove for: " + info1.Name() + " (" + path1 + ")")

		for path2, info2 := range creates {
			w.logger.Info("Check create for: " + info2.Name() + " (" + path2 + ")")
			w.logger.Info("Comparing: " + path1 + " " + path2)
			w.logger.Info(filepath.Dir(path1), filepath.Dir(path2))
			w.logger.Info(info1.Name(), info2.Name())
			w.logger.Info(info1.ModTime().String(), info2.ModTime().String())
			w.logger.Info(info1.Mode().String(), info2.Mode().String())
			w.logger.Info(strconv.FormatInt(info1.Size(), 10), strconv.FormatInt(info2.Size(), 10))
			w.logger.Info("Same file: " + strconv.FormatBool(sameFile(info1, info2)))

			if sameFile(info1, info2) {
				e := Event{
					Op:       Move,
					Path:     path2,
					OldPath:  path1,
					FileInfo: info1,
				}
				// If they are from the same directory, it's a rename
				// instead of a move event.
				if filepath.Dir(path1) == filepath.Dir(path2) {
					e.Op = Rename
				}

				w.logger.Info(e.Op.String() + " for file: " + info1.Name())

				delete(removes, path1)
				delete(creates, path2)

				select {
				case <-cancel:
					return
				case evt <- e:
				}
			}
		}
	}

	// Send all the remaining create and remove events.
	for path, info := range creates {
		select {
		case <-cancel:
			return
		case evt <- Event{Create, path, "", info}:
		}
	}
	for path, info := range removes {
		select {
		case <-cancel:
			return
		case evt <- Event{Remove, path, path, info}:
		}
	}
}

// Wait blocks until the watcher is started.
func (w *Watcher) Wait() {
	w.wg.Wait()
}

// Close stops a Watcher and unlocks its mutex, then sends a close signal.
func (w *Watcher) Close() {
	w.logger.Info("Closing file watcher")

	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	w.running = false
	w.files = make(map[string]os.FileInfo)
	w.names = make(map[string]bool)
	w.mu.Unlock()
	// Send a close signal to the Start method.
	w.close <- struct{}{}
}
