package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mholt/archiver"
	"github.com/taskcluster/httpbackoff"
	"github.com/taskcluster/slugid-go/slugid"
	"github.com/taskcluster/taskcluster-base-go/scopes"
)

var (
	// downloaded files that may be archives or individual files are stored in
	// fileCache, against a unique key that identifies where they were
	// downloaded from. The map values are the paths of the downloaded files
	// relative to the downloads directory specified in the global config file
	// on the worker.
	fileCaches map[string]string = map[string]string{}
	// writable directory caches that may be preloaded or initially empty. Note
	// a preloaded cache will have an associated file cache for the archive it
	// was created from. The key is the cache name.
	directoryCaches map[string]string = map[string]string{}
)

// Represents the Mounts feature as a whole - one global instance
type MountsFeature struct {
}

func (feature *MountsFeature) Initialise() error {
	err := ensureEmptyDir(config.CachesDir)
	if err != nil {
		return err
	}
	return ensureEmptyDir(config.DownloadsDir)
}

func ensureEmptyDir(dir string) error {
	err := os.MkdirAll(dir, 0777)
	if err != nil {
		return err
	}
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, file := range files {
		err := os.RemoveAll(filepath.Join(dir, file.Name()))
		if err != nil {
			return err
		}
	}
	return nil
}

// Represents the Mounts feature for an individual task (one per task)
type TaskMount struct {
	task   *TaskRun
	mounts []MountEntry
	// payload errors are detected when creating feature but only reported when
	// feature starts, so need to keep hold of any error raised...
	payloadError   error
	requiredScopes scopes.Required
}

// Represents an individual Mount listed in task payload - there
// can be several mounts per task
type MountEntry interface {
	Mount() error
	Unmount() error
	FSContent() (FSContent, error)
	RequiredScopes() []string
}

// FSContent represents file system content - it is based on the auto-generated
// type Content which is json.RawMessage, which can be ArtifactContent or
// URLContent concrete types. This is the interface which represents these
// underlying concrete types.
type FSContent interface {
	// Keep it simple and just return a []string, rather than scopes.Required
	// since currently no easy way to "AND" scopes.Required types.
	RequiredScopes() []string
	// Download the content, and return the absolute location of the file. No
	// archive extraction is performed.
	Download() (string, error)
	// UniqueKey returns a string which represents the content, such that if
	// two FSContent types return the same key, it can be assumed they
	// represent the same content.
	UniqueKey() string
}

// No scopes required
func (ac *URLContent) RequiredScopes() []string {
	return []string{}
}

// Scopes queue:get-artifact:<artifact-name> required for non public/ artifacts
func (ac *ArtifactContent) RequiredScopes() []string {
	if strings.HasPrefix(ac.Artifact, "public/") {
		return []string{}
	}
	return []string{"queue:get-artifact:" + ac.Artifact}
}

// Since mounts are protected by scopes per mount, no reason to have
// a feature flag to enable. Having mounts in the payload is enough.
func (feature *MountsFeature) IsEnabled(fl EnabledFeatures) bool {
	return true
}

// Reads payload and initialises state...
func (feature *MountsFeature) NewTaskFeature(task *TaskRun) TaskFeature {
	tm := &TaskMount{
		task:   task,
		mounts: []MountEntry{},
	}
	for i, taskMount := range task.Payload.Mounts {
		// Each mount must be one of:
		//   * WritableDirectoryCache
		//   * ReadOnlyDirectory
		//   * FileMount
		// We have to check keys to find out...
		var m map[string]interface{}
		if err := json.Unmarshal(taskMount, &m); err != nil {
			tm.payloadError = fmt.Errorf("Could not read task mount %v: %v\n%v", i, string(taskMount), err)
			return tm
		}
		switch {
		case m["cacheName"] != nil:
			tm.Unmarshal(taskMount, &WritableDirectoryCache{})
		case m["directory"] != nil:
			tm.Unmarshal(taskMount, &ReadOnlyDirectory{})
		case m["file"] != nil:
			tm.Unmarshal(taskMount, &FileMount{})
		default:
			tm.payloadError = fmt.Errorf("Unrecognised mount entry in payload - %#v", m)
		}
	}
	tm.initRequiredScopes()
	return tm
}

// Utility method to unmarshal a json blob and add it to the mounts in the TaskMount
func (tm *TaskMount) Unmarshal(rm json.RawMessage, m MountEntry) {
	// only update if nil, otherwise we could replace a previous error with nil
	if tm.payloadError == nil {
		tm.payloadError = json.Unmarshal(rm, m)
		tm.mounts = append(tm.mounts, m)
	}
}

// Note, we've calculated the required scopes in NewTaskFeature(...) already -
// we do this in advance in case there is an error, we can report it upfront
// when we initialise, rather than later when we go to check what scopes are
// needed.
func (taskMount *TaskMount) RequiredScopes() scopes.Required {
	return taskMount.requiredScopes
}

// loops through all referenced mounts and checks what scopes are required to
// mount them
func (taskMount *TaskMount) initRequiredScopes() {
	requiredScopes := []string{}
	for _, mount := range taskMount.mounts {
		requiredScopes = append(requiredScopes, mount.RequiredScopes()...)
		fsContent, err := mount.FSContent()
		if err != nil {
			taskMount.payloadError = err
			return
		}
		// A writable cache might not be preloaded so might have no initial content
		if fsContent != nil {
			requiredScopes = append(requiredScopes, fsContent.RequiredScopes()...)
		}
	}
	taskMount.requiredScopes = scopes.Required{requiredScopes}
}

// called when a task starts
func (taskMount *TaskMount) Start() error {
	if taskMount.payloadError != nil {
		return taskMount.payloadError
	}
	// loop through all mounts described in payload
	for _, mount := range taskMount.mounts {
		err := mount.Mount()
		if err != nil {
			return err
		}
	}
	return nil
}

// called when a task has completed
func (taskMount *TaskMount) Stop() error {
	// loop through all mounts described in payload
	for _, mount := range taskMount.mounts {
		err := mount.Unmount()
		if err != nil {
			return err
		}
	}
	return nil
}

// Writable caches require scope generic-worker:cache:<cacheName>. Preloaded caches
// from an artifact may also require scopes - handled separately.
func (w *WritableDirectoryCache) RequiredScopes() []string {
	return []string{"generic-worker:cache:" + w.CacheName}
}

// Returns either a *URLContent or *ArtifactContent that is listed in the given
// *WritableDirectoryCache
func (w *WritableDirectoryCache) FSContent() (FSContent, error) {
	// no content if an empty cache folder, e.g. object directory
	if w.Content != nil {
		return w.Content.FSContent()
	}
	return nil, nil
}

// No scopes directly required for a ReadOnlyDirectory (scopes may be required
// for its content though - handled separately)
func (r *ReadOnlyDirectory) RequiredScopes() []string {
	return []string{}
}

// Returns either a *URLContent or *ArtifactContent that is listed in the given
// *ReadOnlyDirectory
func (r *ReadOnlyDirectory) FSContent() (FSContent, error) {
	return r.Content.FSContent()
}

// No scopes directly required for a FileMount (scopes may be required for its
// content though - handled separately)
func (f *FileMount) RequiredScopes() []string {
	return []string{}
}

// Returns either a *URLContent or *ArtifactContent that is listed in the given
// *FileMount
func (f *FileMount) FSContent() (FSContent, error) {
	return f.Content.FSContent()
}

func (w *WritableDirectoryCache) Mount() error {
	// cache already there?
	if _, dirCacheExists := directoryCaches[w.CacheName]; dirCacheExists {
		// just move it into place...
		err := os.Rename(directoryCaches[w.CacheName], filepath.Join(TaskUser.HomeDir, w.Directory))
		if err != nil {
			return fmt.Errorf("Not able to rename dir: %v", err)
		}
		return nil
	}
	// preloaded content?
	if w.Content != nil {
		c, err := w.Content.FSContent()
		if err != nil {
			return fmt.Errorf("Not able to retrieve FSContent: %v", err)
		}
		err = extract(c, w.Format, filepath.Join(TaskUser.HomeDir, w.Directory))
		if err != nil {
			return err
		}
		return nil
	}
	// no cache, no preloaded content => just create dir in place
	err := os.MkdirAll(filepath.Join(TaskUser.HomeDir, w.Directory), 0777)
	if err != nil {
		return fmt.Errorf("Not able to create dir: %v", err)
	}
	return nil
}

func (r *ReadOnlyDirectory) Mount() error {
	c, err := r.Content.FSContent()
	if err != nil {
		return fmt.Errorf("Not able to retrieve FSContent: %v", err)
	}
	return extract(c, r.Format, filepath.Join(TaskUser.HomeDir, r.Directory))
}

func (f *FileMount) Mount() error {
	c, err := f.Content.FSContent()
	if err != nil {
		return err
	}
	return mountFile(c, filepath.Join(TaskUser.HomeDir, f.File))
}

func (w *WritableDirectoryCache) Unmount() error {
	basename := slugid.Nice()
	file := filepath.Join(config.CachesDir, basename)
	directoryCaches[w.CacheName] = file
	log.Printf("Moving %q to %q", filepath.Join(TaskUser.HomeDir, w.Directory), file)
	return os.Rename(filepath.Join(TaskUser.HomeDir, w.Directory), file)
}

// Nothing to do - original archive file wasn't moved
func (r *ReadOnlyDirectory) Unmount() error {
	return nil
}

func (f *FileMount) Unmount() error {
	fsContent, err := f.FSContent()
	if err != nil {
		return err
	}
	log.Printf("Moving %q to %q", filepath.Join(TaskUser.HomeDir, f.File), fileCaches[fsContent.UniqueKey()])
	return os.Rename(filepath.Join(TaskUser.HomeDir, f.File), fileCaches[fsContent.UniqueKey()])
}

// ensureCached returns a file containing the given content
func ensureCached(fsContent FSContent) (file string, err error) {
	cacheKey := fsContent.UniqueKey()
	if _, inCache := fileCaches[cacheKey]; !inCache {
		file, err := fsContent.Download()
		if err != nil {
			return "", err
		}
		fileCaches[cacheKey] = file
	}
	return fileCaches[cacheKey], nil
}

func mountFile(fsContent FSContent, file string) error {
	cacheFile, err := ensureCached(fsContent)
	if err != nil {
		return err
	}
	parentDir := filepath.Dir(file)
	err = os.MkdirAll(parentDir, 0777)
	if err != nil {
		return err
	}
	err = os.Rename(cacheFile, file)
	if err != nil {
		return fmt.Errorf("Could not rename file %v as %v due to %v", cacheFile, file, err)
	}
	return nil
}

func extract(fsContent FSContent, format string, dir string) error {
	cacheFile, err := ensureCached(fsContent)
	if err != nil {
		return err
	}
	err = os.MkdirAll(dir, 0777)
	if err != nil {
		return err
	}
	switch format {
	case "zip":
		return archiver.Unzip(cacheFile, dir)
	case "tar.gz":
		return archiver.UntarGz(cacheFile, dir)
	case "rar":
		return archiver.Unrar(cacheFile, dir)
	case "tar.bz2":
		return archiver.UntarBz2(cacheFile, dir)
	}
	log.Fatalf("Unsupported format %v", format)
	return nil
}

// Returns either a *ArtifactContent or *URLContent based on the content
// (json.RawMessage)
func (c Content) FSContent() (FSContent, error) {
	// c must be one of:
	//   * ArtifactContent
	//   * URLContent
	// We have to check keys to find out...
	var m map[string]interface{}
	if err := json.Unmarshal(c, &m); err != nil {
		return nil, err
	}
	switch {
	case m["artifact"] != nil:
		return c.Unmarshal(&ArtifactContent{})
	case m["url"] != nil:
		return c.Unmarshal(&URLContent{})
	}
	return nil, errors.New("Unrecognised mount entry in payload")
}

// Utility method to unmarshal Content (json.RawMessage) into *ArtifactContent
// or *URLContent (or anything that implements FSContent interface)
func (c Content) Unmarshal(fsContent FSContent) (FSContent, error) {
	err := json.Unmarshal(c, fsContent)
	return fsContent, err
}

// Downloads ArtifactContent to a file inside the downloads directory specified
// in the global config file. The filename is a random slugid, and the
// absolute path of the file is returned.
func (ac *ArtifactContent) Download() (string, error) {
	basename := slugid.Nice()
	file := filepath.Join(config.DownloadsDir, basename)
	signedURL, err := Queue.GetLatestArtifact_SignedURL(ac.TaskID, ac.Artifact, time.Minute*30)
	if err != nil {
		return "", err
	}
	return file, downloadURLToFile(signedURL.String(), file)
}

func (ac *ArtifactContent) UniqueKey() string {
	return "artifact:" + ac.TaskID + ":" + ac.Artifact
}

// Downloads URLContent to a file inside the caches directory specified in the
// global config file.  The filename is a random slugid, and the absolute path
// of the file is returned.
func (uc *URLContent) Download() (string, error) {
	basename := slugid.Nice()
	file := filepath.Join(config.DownloadsDir, basename)
	return file, downloadURLToFile(uc.URL, file)
}

func (uc *URLContent) UniqueKey() string {
	return "urlcontent:" + uc.URL
}

// Utility function to aggressively download a url to a file location
func downloadURLToFile(url, file string) error {
	log.Printf("Downloading url %v to %v", url, file)
	err := os.MkdirAll(filepath.Dir(file), 0777)
	if err != nil {
		return err
	}
	resp, _, err := httpbackoff.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 0600 so other tasks can't read content! Let's hope this also works on Windows...
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	if err != nil {
		return err
	}
	return nil
}
