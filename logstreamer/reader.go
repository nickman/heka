/***** BEGIN LICENSE BLOCK *****
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at http://mozilla.org/MPL/2.0/.
#
# The Initial Developer of the Original Code is the Mozilla Foundation.
# Portions created by the Initial Developer are Copyright (C) 2012
# the Initial Developer. All Rights Reserved.
#
# Contributor(s):
#   Ben Bangert (bbangert@mozilla.com)
#
# ***** END LICENSE BLOCK *****/

package logstreamer

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mozilla-services/heka/ringbuf"
	"io"
	"os"
)

// A location in a logstream indicating the farthest that has been read
type LogstreamLocation struct {
	Filename     string           `json:"file_name"`
	SeekPosition int64            `json:"seek"`
	Hash         string           `json:"last_hash"`
	JournalPath  string           `json:"-"`
	lastLine     *ringbuf.Ringbuf `json:"-"`
}

var LINEBUFFERLEN = 500

// Loads a logstreamlocation from a file or returns an empty one if no journal
// record was found.
func LogstreamLocationFromFile(path string) (l *LogstreamLocation, err error) {
	l = new(LogstreamLocation)
	l.JournalPath = path
	l.lastLine = ringbuf.New(LINEBUFFERLEN)

	// So that we can check to see if it exists or not
	var seekJournal *os.File
	if seekJournal, err = os.Open(l.JournalPath); err != nil {
		// The logfile doesn't exist, nothing special to do
		if os.IsNotExist(err) {
			// file doesn't exist, but that's ok, not a real error
			err = nil
		}
		return
	}
	contents := bytes.NewBuffer(nil)
	defer seekJournal.Close()
	io.Copy(contents, seekJournal)

	defer func() {
		if r := recover(); r != nil {
			err = errors.New("Error parsing the journal file")
		}
	}()

	err = json.Unmarshal(contents.Bytes(), l)
	return
}

// If the buffer is large enough, generate a hash value in the position
func (l *LogstreamLocation) GenerateHash() {
	if l.lastLine.Size() == LINEBUFFERLEN {
		lastLine := make([]byte, LINEBUFFERLEN)
		n := l.lastLine.Read(lastLine)
		logline := string(lastLine[:n])

		if logline != "" {
			h := sha1.New()
			io.WriteString(h, logline)
			l.Hash = fmt.Sprintf("%x", h.Sum(nil))
		}
	}
}

func (l *LogstreamLocation) Reset() {
	l.Filename = ""
	l.SeekPosition = int64(0)
	l.Hash = ""
	l.lastLine = ringbuf.New(LINEBUFFERLEN)
}

func (l *LogstreamLocation) Save() error {
	// If we don't have a JournalPath, ignore
	if l.JournalPath == "" {
		return nil
	}

	// Don't save if we had a prior has and haven't read more than
	// LINEBUFFERLEN bytes into the file
	if l.lastLine.Size() < LINEBUFFERLEN {
		return nil
	}

	l.GenerateHash()

	b, err := json.Marshal(l)
	if err != nil {
		return err
	}

	seekJournal, file_err := os.OpenFile(l.JournalPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0660)
	if file_err != nil {
		return fmt.Errorf("Error opening seek recovery log: %s", file_err.Error())
	}
	defer seekJournal.Close()

	if _, file_err = seekJournal.Write(b); file_err != nil {
		return fmt.Errorf("Error writing seek recovery log: %s", file_err.Error())
	}
	return nil
}

// Determine if a newer file is available, if it is, return the filename of it.
func (l *Logstream) NewerFileAvailable() (file string, ok bool) {
	/* Formula for determining if a newer file is available
		   1. Are we the file we think we are?
		       NO - Find out what file we are, if we're unable to locate where we
		            are and there's logfiles, there is a newer file available
	                (the oldest).
	                If we can locate where we are, update our filename with our
	                new filename and proceed to Step 2.
		       YES - Step 2
		   2. Is there a newer file in our list ahead of us?
		       NO - No newer file available.
		       YES - return ok and the new filename.
	*/
	currentInfo, err := l.fd.Stat()
	if err != nil {
		return "", false
	}
	fInfo, err := os.Stat(l.position.Filename)
	if err != nil {
		return "", false
	}

	// 1. If our size is greater than the file at this filename, we're not the
	// same file
	if currentInfo.Size() >= fInfo.Size() {
		ok = true
	} else if !l.VerifyFileHash() {
		// Our file-hash didn't verify, not the same file
		ok = true
	}

	if ok {
		// 1. NO - Try and find our location
		l.position.Filename = ""
		fd, err := l.LocatePriorLocation()
		fd.Close()

		// Unable to locate prior position in our file-stream, are there
		// any logfiles?
		if err != nil {
			l.lfMutex.RLock()
			defer l.lfMutex.RUnlock()
			if len(l.logfiles) > 0 {
				file = l.logfiles[0].FileName
				return
			} else {
				// Apparently no logfiles at all, retain this fd
				ok = false
				return
			}
		}

		// We were able to locate our prior location, our filename was
		// updated
		ok = false
	}

	// 2. Newer file ahead of us?
	l.lfMutex.RLock()
	defer l.lfMutex.RUnlock()
	fileIndex := l.logfiles.IndexOf(l.position.Filename)
	if fileIndex == -1 {
		// We couldn't find our filename in the list? Then there's nothing
		// newer
		return
	}

	if fileIndex+1 < len(l.logfiles) {
		// There's a newer file!
		return l.logfiles[fileIndex+1].FileName, true
	}

	return
}

// Verify the position in the file is still at that position in that file (ie,
// the file has not been moved in some fashion.)
// Returns false if the file of this position does not match, True otherwise
func (l *Logstream) VerifyFileHash() bool {
	fd, err := os.Open(l.position.Filename)
	if err != nil {
		return true
	}
	defer fd.Close()

	// Try to get to our seek position.
	if _, err = fd.Seek(l.position.SeekPosition-int64(LINEBUFFERLEN), 0); err == nil {
		// We should be at the beginning of the last line read the last
		// time Heka ran.
		reader := bufio.NewReader(fd)
		buf := make([]byte, LINEBUFFERLEN)
		_, err := io.ReadAtLeast(reader, buf, LINEBUFFERLEN)
		if err == nil {
			h := sha1.New()
			h.Write(buf)
			tmp := fmt.Sprintf("%x", h.Sum(nil))
			if tmp == l.position.Hash {
				return true
			}
		}
	}
	return false
}

// Locate and return a file handle seeked to the appropriate location. An error will be
// returned if the prior location cannot be located.
// If the logfile this location for has changed names, the position will be updated to
// reflect the move.
func (l *Logstream) LocatePriorLocation() (fd *os.File, err error) {
	var info os.FileInfo
	l.lfMutex.RLock()
	defer l.lfMutex.RUnlock()

	fileIndex := l.logfiles.IndexOf(l.position.Filename)
	if fileIndex != -1 {
		fd, err = SeekInFile(l.position.Filename, l.position)
		if err == nil {
			return
		}
		// Check to see whether its a file permission error, return if it is
		if os.IsPermission(err) {
			return
		}
		err = nil // Reset our error to nil
	}

	// Unable to locate the file, or the position wasn't where we thought it should be.
	// Start systematically searching all the files for this location to see if it was
	// shuffled around.
	// TODO: Would be more efficient to start searching backwards from where we are
	//       in the logstream at the moment.
	for _, logfile := range l.logfiles {
		// Check that the file is large enough for our seek position
		info, err = os.Stat(logfile.FileName)
		if err != nil {
			return
		}
		if info.Size() < l.position.SeekPosition {
			continue
		}

		fd, err = SeekInFile(logfile.FileName, l.position)
		if err == nil {
			// Located the position! Update the filename in the position
			l.position.Filename = logfile.FileName
			return
		}
		// Check to see whether its a file permission error, return if it is
		if os.IsPermission(err) {
			return
		}
		err = nil // Reset our error to nil
	}
	return
}

// Seek into a file, return an error if a match wasn't found
func SeekInFile(path string, position *LogstreamLocation) (fd *os.File, err error) {
	if fd, err = os.Open(path); err != nil {
		return
	}

	// Try to get to our seek position.
	if _, err = fd.Seek(position.SeekPosition-int64(LINEBUFFERLEN), 0); err == nil {
		// We should be at the beginning of the last line read the last
		// time Heka ran.
		reader := bufio.NewReader(fd)
		buf := make([]byte, LINEBUFFERLEN)
		_, err := io.ReadAtLeast(reader, buf, LINEBUFFERLEN)
		if err == nil {
			h := sha1.New()
			h.Write(buf)
			tmp := fmt.Sprintf("%x", h.Sum(nil))
			if tmp == position.Hash {
				position.lastLine.Write(buf)
				return fd, nil
			}
		}
	}
	return nil, errors.New("Unable to locate position")
}

// TODO:: Refactor into a different heka package for use by all plugins
// and have PluginRunner inherit from it
type Logger interface {
	LogError(err error)
	LogMessage(msg string)
}

func (l *Logstream) Read(p []byte) (n int, err error) {
	// Do we have a file descriptor already?
	fd := l.fd

	// If we have a fd, read it
	if fd != nil {
		return l.readBytes(p)
	}

	// This is a fresh read attempt with no existing file descriptor
	// If we have a position, attempt to restore it
	if l.position.Filename != "" {
		if fd, err = l.LocatePriorLocation(); err != nil {
			return
		} else {
			l.fd = fd
		}
	} else {
		// No position to recover from, use oldest file if there is one
		if len(l.logfiles) < 1 {
			return 0, errors.New("No files found to read from")
		}

		// Reset the position, attempt to start in the oldest file
		l.position.Reset()
		l.position.Filename = l.logfiles[0].FileName
		if fd, err = l.LocatePriorLocation(); err != nil {
			return
		}
		l.fd = fd
	}

	return l.readBytes(p)
}

// Called to actually read from the file descriptor if possible
func (l *Logstream) readBytes(p []byte) (n int, err error) {
	// Before we read, we check to see if there's a newer file
	// If there is a newer file, then we know that if we hit EOF here
	// the new one has already started getting data so its safe to move
	// on. If we did this check after hitting EOF, then its possible we
	// could move on without doing a last read of the fd.
	var (
		newerFilename string
		ok            bool
	)

	// If we had an EOF last time, we check for a new file before trying
	// to read again
	if l.priorEOF {
		newerFilename, ok = l.NewerFileAvailable()
	}

	// We're ready to read, commit the read and update our position
	n, err = l.fd.Read(p)

	if err != io.EOF {
		// Some unexpected error, reset everything
		// but don't kill the watcher
		l.fd.Close()
		if l.fd != nil {
			l.fd = nil
		}
		l.position.Reset()
		return
	}

	if n > 0 {
		l.position.SeekPosition += int64(n)
		l.position.lastLine.Write(p[:n])
	}

	// We previously got an EOF, but not this time, so reset
	if err != io.EOF && l.priorEOF {
		l.priorEOF = false
	}

	// Got an EOF, but this is the first time around, check for a
	// newer file on the next time around
	if err == io.EOF && !l.priorEOF {
		l.priorEOF = true
	} else if err == io.EOF && l.priorEOF {
		// Another EOF, so we can determine if there's a newer file
		err = nil
		// We hit EOF, and we don't have a newer file, so we will keep
		// checking for a newer file
		if !ok {
			return
		}

		// We do have a new file, grab the file handle first
		var fd *os.File
		fd, err = os.Open(newerFilename)
		if err != nil {
			// Return the error, keep our existing handle
			fd.Close()
			return
		}

		// Verify that our newerFilename is still what we think it should
		// be and our files didn't move around between calls, if we were
		// rotated after the other NewerFileAvailable call then the filename
		// here will be different
		verifyFilename, vOk := l.NewerFileAvailable()
		if verifyFilename != newerFilename || !vOk {
			fd.Close()
			return
		}

		// Ok, we have the handle for the right file, even if it might've
		// been rotated by now
		l.fd.Close()
		l.position.Reset()
		l.position.Filename = newerFilename
		l.fd = fd
		l.priorEOF = false
	}
	return
}
