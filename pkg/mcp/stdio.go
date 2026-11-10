package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
)

// Stdio is the stdio transport of a server post reaches: a JSON message on
// each line of in, each reply on a line of out, until in ends. post returns
// the reply, nil for none. Only protocol goes to out; logs go to stderr.
func Stdio(ctx context.Context, in io.Reader, out io.Writer, post func(context.Context, []byte) ([]byte, error)) error {
	br := bufio.NewReaderSize(in, 64<<10)
	for ctx.Err() == nil {
		line, over, err := readLine(br)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		var reply []byte
		if over {
			reply = encode(failure(noID, codeInvalid, "a message is at most %d bytes", MaxBody))
		} else {
			reply = forward(ctx, line, post)
		}
		if reply != nil {
			if _, werr := out.Write(append(reply, '\n')); werr != nil {
				return werr
			}
		}
		if err != nil { // the end of in, after what it held
			return nil
		}
	}
	return ctx.Err()
}

// readLine is the next line, without its end. over reports a line longer than
// MaxBody, which is read past and not kept.
func readLine(br *bufio.Reader) (line []byte, over bool, err error) {
	for {
		part, err := br.ReadSlice('\n')
		if !over {
			line = append(line, part...)
			if len(line) > MaxBody+1 { // +1 for the line's end
				line, over = nil, true
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, over, err
	}
}

// forward posts one line and is the line to write back, nil for none. What
// is not a message stops here, since a refusal over HTTP would lose its id.
func forward(ctx context.Context, line []byte, post func(context.Context, []byte) ([]byte, error)) []byte {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	if !json.Valid(line) {
		return encode(failure(noID, codeParse, "parse error: not JSON"))
	}
	var in struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(line, &in); err != nil {
		return encode(failure(noID, codeInvalid, "invalid request: a message is one JSON-RPC object, and batches are not served"))
	}
	reply, err := post(ctx, line)
	if err == nil && len(reply) > 0 {
		var compact bytes.Buffer
		if err = json.Compact(&compact, reply); err == nil {
			return compact.Bytes()
		}
		err = fmt.Errorf("pai-server answered with something that is not JSON: %w", err)
	}
	if err == nil {
		return nil
	}
	if in.ID == nil { // a notification has no one to tell
		log.Printf("mcp: %v", err)
		return nil
	}
	return encode(failure(in.ID, codeServer, "%v", err))
}
