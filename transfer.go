// Copyright 2015 Muir Manders.  All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package goftp

import (
	"fmt"
	"io"
	"os"
	"strconv"
)

// Retrieve file "path" from server and write bytes to "dest". If the
// server supports resuming stream transfers, Retrieve will continue
// resuming a failed download as long as it continues making progress.
// Retrieve will also verify the file's size after the transfer if the
// server supports the SIZE command.
func (c *Client) Retrieve(path string, dest io.Writer) error {
	return c.RetrieveFrom(path, dest, 0)
}

// RetrieveFrom copies the contents of file "path" from "offset" onwards
// into "dest", using the same resumption as Retrieve for the remainder.
//
// An offset equal to the file's size is not an error: it copies nothing,
// which is the honest answer for a caller resuming a transfer that had
// already finished. An offset beyond the size is a mistake worth
// reporting rather than answering with silence.
//
// The server must support resuming stream transfers, which is the same
// requirement Retrieve has for resuming after a failure. Where Retrieve
// merely loses the ability to retry, RetrieveFrom cannot start at all —
// so it says so instead of quietly sending the whole file.
func (c *Client) RetrieveFrom(path string, dest io.Writer, offset int64) error {
	if offset < 0 {
		return ftpError{err: fmt.Errorf("offset %d is negative", offset)}
	}

	// fetch file size to check against how much we transferred
	size, err := c.size(path)
	if err != nil {
		return err
	}

	if size != -1 && offset > size {
		return ftpError{err: fmt.Errorf(
			"offset %d is past the end of %q, which is %d bytes", offset, path, size)}
	}

	canResume := c.canResume()

	if offset > 0 && !canResume {
		return ftpError{err: fmt.Errorf(
			"cannot start at offset %d: the server does not support REST STREAM", offset)}
	}

	// Nothing to copy, and asking the server for nothing invites a
	// server-specific answer to a question with an obvious one.
	if offset == size {
		return nil
	}

	bytesSoFar := offset
	for {
		n, err := c.transferFromOffset(path, dest, nil, bytesSoFar)

		bytesSoFar += n

		if err == nil {
			break
		} else if n == 0 {
			return err
		} else if !canResume {
			return ftpError{
				err:       fmt.Errorf("%s (can't resume)", err),
				temporary: true,
			}
		}
	}

	// bytesSoFar counts from the start of the file, not from the offset,
	// because that is what REST resumes against — so the total to expect
	// is still the file's size.
	if size != -1 && bytesSoFar != size {
		return ftpError{
			err:       fmt.Errorf("expected %d bytes, got %d", size, bytesSoFar),
			temporary: true,
		}
	}

	return nil
}

// SupportsRestart reports whether the server can start a transfer part way
// into a file (FEAT lists "REST STREAM"), which RetrieveFrom and
// RetrieveRange need. A server that can't is not asked to.
func (c *Client) SupportsRestart() bool { return c.canResume() }

// RetrieveRange copies up to length bytes of file "path", starting at
// "offset", into "dest", and returns how many it copied. It is for reading a
// part of a file: the data connection is closed once length bytes have
// arrived, so a server that has more to send is told to stop (it answers
// 426) and the rest of the file is not transferred. A range that runs past
// the end of the file gives the bytes there are, as io.ReaderAt does with
// io.EOF - here with no error: the caller compares the count with length.
//
// Like RetrieveFrom it needs REST STREAM for any offset above zero.
func (c *Client) RetrieveRange(path string, dest io.Writer, offset, length int64) (int64, error) {
	if offset < 0 || length < 0 {
		return 0, ftpError{err: fmt.Errorf("range %d+%d is negative", offset, length)}
	}
	if length == 0 {
		return 0, nil
	}
	if offset > 0 && !c.canResume() {
		return 0, ftpError{err: fmt.Errorf(
			"cannot start at offset %d: the server does not support REST STREAM", offset)}
	}

	pconn, err := c.getIdleConn()
	if err != nil {
		return 0, err
	}
	defer c.returnConn(pconn)

	if err = pconn.setType("I"); err != nil {
		return 0, err
	}
	if offset > 0 {
		if err := pconn.sendCommandExpected(replyFileActionPending, "REST %d", offset); err != nil {
			return 0, err
		}
	}
	connGetter, abort, err := pconn.prepareDataConn()
	if err != nil {
		return 0, err
	}
	defer abort()

	if err = pconn.sendCommandExpected(replyGroupPreliminaryReply, "RETR %s", path); err != nil {
		return 0, err
	}
	dc, err := connGetter()
	if err != nil {
		return 0, err
	}
	defer dc.Close()

	n, err := io.CopyN(dest, dc, length)
	if err != nil && err != io.EOF {
		pconn.broken = true
		return n, err
	}
	// closing the data connection is how the transfer is ended early; the
	// server's answer is then 426 (aborted) or, if it had finished, 226
	if cerr := dc.Close(); cerr != nil {
		pconn.debug("error closing data connection: %s", cerr)
	}
	code, msg, rerr := pconn.readResponse()
	switch {
	case rerr != nil:
		// the bytes were read; the control connection is no longer trusted
		// (readResponse has marked it broken) and is replaced
		pconn.debug("no answer after the range: %s", rerr)
	case positiveCompletionReply(code) || code == 426 || code == 450 || code == 451:
	default:
		pconn.debug("unexpected response after RETR of a range: %d (%s)", code, msg)
		return n, ftpError{code: code, msg: msg}
	}
	return n, nil
}

// Store bytes read from "src" into file "path" on the server. If the
// server supports resuming stream transfers and "src" is an io.Seeker
// (*os.File is an io.Seeker), Store will continue resuming a failed upload
// as long as it continues making progress. Store will not attempt to
// resume an upload if the client is connected to multiple servers. Store
// will also verify the remote file's size after the transfer if the server
// supports the SIZE command.
func (c *Client) Store(path string, src io.Reader) error {

	canResume := len(c.hosts) == 1 && c.canResume()

	seeker, ok := src.(io.Seeker)
	if !ok {
		canResume = false
	}

	var (
		bytesSoFar int64
		err        error
		n          int64
	)
	for {
		if bytesSoFar > 0 {
			size, sizeErr := c.size(path)
			if sizeErr != nil {
				return ftpError{
					err:       sizeErr,
					temporary: true,
				}
			}
			if size == -1 {
				return ftpError{
					err:       fmt.Errorf("%s (resume failed)", err),
					temporary: true,
				}
			}

			_, seekErr := seeker.Seek(size, os.SEEK_SET)
			if seekErr != nil {
				c.debug("failed seeking to %d while resuming upload to %s: %s",
					size,
					path,
					err,
				)
				return ftpError{
					err:       fmt.Errorf("%s (resume failed)", err),
					temporary: true,
				}
			}
			bytesSoFar = size
		}

		n, err = c.transferFromOffset(path, nil, src, bytesSoFar)

		bytesSoFar += n

		if err == nil {
			break
		} else if n == 0 {
			return ftpError{
				err:       err,
				temporary: true,
			}
		} else if !canResume {
			return ftpError{
				err:       fmt.Errorf("%s (can't resume)", err),
				temporary: true,
			}
		}
	}

	// fetch file size to check against how much we transferred
	size, err := c.size(path)
	if err != nil {
		return err
	}
	if size != -1 && size != bytesSoFar {
		return ftpError{
			err:       fmt.Errorf("sent %d bytes, but size is %d", bytesSoFar, size),
			temporary: true,
		}
	}

	return nil
}

func (c *Client) transferFromOffset(path string, dest io.Writer, src io.Reader, offset int64) (int64, error) {
	pconn, err := c.getIdleConn()
	if err != nil {
		return 0, err
	}

	defer c.returnConn(pconn)

	if err = pconn.setType("I"); err != nil {
		return 0, err
	}

	if offset > 0 {
		err := pconn.sendCommandExpected(replyFileActionPending, "REST %d", offset)
		if err != nil {
			return 0, err
		}
	}

	connGetter, abort, err := pconn.prepareDataConn()
	if err != nil {
		pconn.debug("error preparing data connection: %s", err)
		return 0, err
	}

	// The data connection is open from here, but nothing closes it until
	// the getter below has handed it over. A transfer command the server
	// refuses returns in between, so without this the connection is lost
	// with no reference left to close it — one descriptor per failed
	// transfer, on both ends.
	defer abort()

	var cmd string
	if dest == nil && src != nil {
		cmd = "STOR"
	} else if dest != nil && src == nil {
		cmd = "RETR"
	} else {
		panic("this shouldn't happen")
	}

	err = pconn.sendCommandExpected(replyGroupPreliminaryReply, "%s %s", cmd, path)
	if err != nil {
		return 0, err
	}

	dc, err := connGetter()
	if err != nil {
		pconn.debug("error getting data connection: %s", err)
		return 0, err
	}

	// to catch early returns
	defer dc.Close()

	if dest == nil {
		dest = dc
	} else {
		src = dc
	}

	n, err := io.Copy(dest, src)

	if err != nil {
		pconn.broken = true
		return n, err
	}

	err = dc.Close()
	if err != nil {
		pconn.debug("error closing data connection: %s", err)
	}

	code, msg, err := pconn.readResponse()
	if err != nil {
		pconn.debug("error reading response after %s: %s", cmd, err)
		return n, err
	}

	if !positiveCompletionReply(code) {
		pconn.debug("unexpected response after %s: %d (%s)", cmd, code, msg)
		return n, ftpError{code: code, msg: msg}
	}

	return n, nil
}

// Fetch SIZE of file. Returns error only on underlying connection error.
// If the server doesn't support size, it returns -1 and no error.
func (c *Client) size(path string) (int64, error) {
	pconn, err := c.getIdleConn()
	if err != nil {
		return -1, err
	}

	defer c.returnConn(pconn)

	if !pconn.hasFeature("SIZE") {
		pconn.debug("server doesn't support SIZE")
		return -1, nil
	}

	if err = pconn.setType("I"); err != nil {
		return 0, err
	}

	code, msg, err := pconn.sendCommand("SIZE %s", path)
	if err != nil {
		return -1, err
	}

	if code != replyFileStatus {
		pconn.debug("unexpected SIZE response: %d (%s)", code, msg)
		return -1, nil
	}

	size, err := strconv.ParseInt(msg, 10, 64)
	if err != nil {
		pconn.debug(`failed parsing SIZE response "%s": %s`, msg, err)
		return -1, nil
	}

	return size, nil
}

func (c *Client) canResume() bool {
	pconn, err := c.getIdleConn()
	if err != nil {
		return false
	}

	defer c.returnConn(pconn)

	return pconn.hasFeatureWithArg("REST", "STREAM")
}
