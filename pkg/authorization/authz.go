package authorization

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/pkg/ioutils"
)

const maxBodySize = 1048576 // 1MB

// NewCtx creates new authZ context, it is used to store authorization information related to a specific docker
// REST http session
// A context provides two method:
// Authenticate Request:
// Call authZ plugins with current REST request and AuthN response
// Request contains full HTTP packet sent to the docker daemon
// https://docs.docker.com/reference/api/docker_remote_api/
//
// Authenticate Response:
// Call authZ plugins with full info about current REST request, REST response and AuthN response
// The response from this method may contains content that overrides the daemon response
// This allows authZ plugins to filter privileged content
//
// If multiple authZ plugins are specified, the block/allow decision is based on ANDing all plugin results
// For response manipulation, the response from each plugin is piped between plugins. Plugin execution order
// is determined according to daemon parameters
func NewCtx(authZPlugins []Plugin, user, userAuthNMethod, requestMethod, requestURI string) *Ctx {
	return &Ctx{
		plugins:         authZPlugins,
		user:            user,
		userAuthNMethod: userAuthNMethod,
		requestMethod:   requestMethod,
		requestURI:      requestURI,
	}
}

// Ctx stores a a single request-response interaction context
type Ctx struct {
	user            string
	userAuthNMethod string
	requestMethod   string
	requestURI      string
	plugins         []Plugin
	// authReq stores the cached request object for the current transaction
	authReq *Request
}

// AuthZRequest authorized the request to the docker daemon using authZ plugins
func (ctx *Ctx) AuthZRequest(w http.ResponseWriter, r *http.Request) error {
	var body []byte
	if sendBody(ctx.requestURI, r.Header) {
		if r.ContentLength < maxBodySize {
			var err error
			body, r.Body, err = drainBody(r.Body)
			if err != nil {
				return err
			}
		}
	}

	var h bytes.Buffer
	if err := r.Header.Write(&h); err != nil {
		return err
	}

	ctx.authReq = &Request{
		User:            ctx.user,
		UserAuthNMethod: ctx.userAuthNMethod,
		RequestMethod:   ctx.requestMethod,
		RequestURI:      ctx.requestURI,
		RequestBody:     body,
		RequestHeaders:  headers(r.Header),
	}

	for _, plugin := range ctx.plugins {
		logrus.Debugf("AuthZ request using plugin %s", plugin.Name())

		authRes, err := plugin.AuthZRequest(ctx.authReq)
		if err != nil {
			return fmt.Errorf("plugin %s failed with error: %s", plugin.Name(), err)
		}

		if !authRes.Allow {
			return fmt.Errorf("authorization denied by plugin %s: %s", plugin.Name(), authRes.Msg)
		}
	}

	return nil
}

// AuthZResponse authorized and manipulates the response from docker daemon using authZ plugins
func (ctx *Ctx) AuthZResponse(rm ResponseModifier, r *http.Request) error {
	ctx.authReq.ResponseStatusCode = rm.StatusCode()
	ctx.authReq.ResponseHeaders = headers(rm.Header())

	if sendBody(ctx.requestURI, rm.Header()) {
		ctx.authReq.ResponseBody = rm.RawBody()
	}

	for _, plugin := range ctx.plugins {
		logrus.Debugf("AuthZ response using plugin %s", plugin.Name())

		authRes, err := plugin.AuthZResponse(ctx.authReq)
		if err != nil {
			return fmt.Errorf("plugin %s failed with error: %s", plugin.Name(), err)
		}

		if !authRes.Allow {
			return fmt.Errorf("authorization denied by plugin %s: %s", plugin.Name(), authRes.Msg)
		}
	}

	rm.FlushAll()

	return nil
}

// drainBody dump the body, it reads the body data into memory and
// see go sources /go/src/net/http/httputil/dump.go
func drainBody(body io.ReadCloser) ([]byte, io.ReadCloser, error) {
	bufReader := bufio.NewReaderSize(body, maxBodySize)
	newBody := ioutils.NewReadCloserWrapper(bufReader, func() error { return body.Close() })

	data, err := bufReader.Peek(maxBodySize)
	if err != io.EOF {
		// This means the request is larger than our max
		if err == bufio.ErrBufferFull {
			return nil, newBody, nil
		}
		// This means we had an error reading
		return nil, nil, err
	}

	return data, newBody, nil
}

// sendBody returns true when request/response body should be sent to AuthZPlugin
func sendBody(url string, header http.Header) bool {
	// Skip body for auth endpoint
	if strings.HasSuffix(url, "/auth") {
		return false
	}

	// body is sent only for text or json messages
	v := header.Get("Content-Type")
	return strings.HasPrefix(v, "text/") || v == "application/json"
}

// headers returns flatten version of the http headers excluding authorization
func headers(header http.Header) map[string]string {
	v := make(map[string]string, 0)
	for k, values := range header {
		// Skip authorization headers
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Registry-Config") || strings.EqualFold(k, "X-Registry-Auth") {
			continue
		}
		for _, val := range values {
			v[k] = val
		}
	}
	return v
}
