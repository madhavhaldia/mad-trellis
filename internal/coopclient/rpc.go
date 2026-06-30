package coopclient

import (
	"errors"
	"regexp"
	"strconv"
)

// params normalizes the params argument so EVERY call sends a JSON object: a
// no-arg call sends {} rather than null. The frozen contract always carries a
// params field; rpcclient drops a nil params, so we substitute an empty map.
func params(p any) any {
	if p == nil {
		return map[string]any{}
	}
	return p
}

// codeSuffix matches the "(code N)" tail that rpcclient appends to a flattened
// protocol error: it formats protocol errors as `rpc <method>: <msg> (code N)`
// and transport errors WITHOUT that suffix. We recover the structured code from
// the suffix so callers can tell a daemon verdict (a real code) from a
// transport/timeout failure (no suffix -> Code 0).
var codeSuffix = regexp.MustCompile(`\(code (-?\d+)\)\s*$`)

// classifyErr converts an rpcclient error into a typed *Error. A protocol error
// (carrying a "(code N)" suffix) keeps that structured code; any other error
// (dial failure, deadline breach, decode error) is a transport failure with
// Code 0, so IsTransport reports it and fail-soft callers degrade to ALLOW.
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if m := codeSuffix.FindStringSubmatch(msg); m != nil {
		if code, perr := strconv.Atoi(m[1]); perr == nil {
			return &Error{Code: code, Message: msg}
		}
	}
	return &Error{Code: 0, Message: msg}
}

// asErr is errors.As with a local alias so callers in this package read
// cleanly; it reports whether err (or a wrapped error) is a *Error.
func asErr(err error, target **Error) bool {
	return errors.As(err, target)
}
