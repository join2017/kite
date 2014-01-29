package kite

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"koding/kite/dnode"
	"koding/kite/dnode/rpc"
	"koding/kite/protocol"
	"strconv"
	"sync"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/op/go-logging"
)

// Default timeout value for RemoteKite.Tell method.
// It can be overriden with RemoteKite.SetTellTimeout.
const DefaultTellTimeout = 4 * time.Second

// RemoteKite is the client for communicating with another Kite.
// It has Tell() and Go() methods for calling methods sync/async way.
type RemoteKite struct {
	// The information about the kite that we are connecting to.
	protocol.Kite

	// A reference to the current Kite running.
	localKite *Kite

	// A reference to the Kite's logger for easy access.
	Log *logging.Logger

	// Credentials that we sent in each request.
	Authentication Authentication

	// dnode RPC client that processes messages.
	client *rpc.Client

	// To signal waiters of Go() on disconnect.
	disconnect chan bool

	// Duration to wait reply from remote when making a request with Tell().
	tellTimeout time.Duration
}

// NewRemoteKite returns a pointer to a new RemoteKite. The returned instance
// is not connected. You have to call Dial() or DialForever() before calling
// Tell() and Go() methods.
func (k *Kite) NewRemoteKite(kite protocol.Kite, auth Authentication) *RemoteKite {
	r := &RemoteKite{
		Kite:           kite,
		localKite:      k,
		Log:            k.Log,
		Authentication: auth,
		client:         k.server.NewClientWithHandlers(),
		disconnect:     make(chan bool),
	}
	r.SetTellTimeout(DefaultTellTimeout)

	// Required for customizing dnode protocol for Kite.
	r.client.SetWrappers(wrapMethodArgs, wrapCallbackArgs, runMethod, runCallback, onError)

	// We need a reference to the local kite when a method call is received.
	r.client.Properties()["localKite"] = k

	// We need a reference to the remote kite when sending a message to remote.
	r.client.Properties()["remoteKite"] = r

	// Add trusted root certificates for client.
	r.client.Config.TlsConfig = &tls.Config{RootCAs: x509.NewCertPool()}
	for _, cert := range k.tlsCertificates {
		r.client.Config.TlsConfig.RootCAs.AppendCertsFromPEM(cert)
	}

	r.OnConnect(func() {
		if r.Authentication.validUntil == nil {
			return
		}

		// Start a goroutine that will renew the token before it expires.
		go r.tokenRenewer()
	})

	var m sync.Mutex
	r.OnDisconnect(func() {
		m.Lock()
		close(r.disconnect)
		r.disconnect = make(chan bool)
		m.Unlock()
	})

	return r
}

func onError(err error) {
	switch e := err.(type) {
	case dnode.MethodNotFoundError: // Tell the requester "method is not found".
		if len(e.Args) == 0 {
			return
		}

		var options callOptions
		if e.Args[0].Unmarshal(&options) != nil {
			return
		}

		if options.ResponseCallback != nil {
			response := callbackArg{
				Result: nil,
				Error:  errorForSending(&Error{"methodNotFound", err.Error()}),
			}
			options.ResponseCallback(response)
		}
	}
}

func wrapCallbackArgs(args []interface{}, tr dnode.Transport) []interface{} {
	return []interface{}{&callOptionsOut{
		WithArgs: args,
		callOptions: callOptions{
			Kite: tr.Properties()["localKite"].(*Kite).Kite,
		},
	}}
}

// newRemoteKiteWithClient returns a pointer to new RemoteKite instance.
// The client will be replaced with the given client.
// Used to give the Kite method handler a working RemoteKite to call methods
// on other side.
func (k *Kite) newRemoteKiteWithClient(kite protocol.Kite, auth Authentication, client *rpc.Client) *RemoteKite {
	r := k.NewRemoteKite(kite, auth)
	r.client = client
	r.client.SetWrappers(wrapMethodArgs, wrapCallbackArgs, runMethod, runCallback, onError)
	r.client.Properties()["localKite"] = k
	r.client.Properties()["remoteKite"] = r
	return r
}

// SetTellTimeout sets the timeout duration for requests made with Tell().
func (r *RemoteKite) SetTellTimeout(d time.Duration) { r.tellTimeout = d }

// Dial connects to the remote Kite. Returns error if it can't.
func (r *RemoteKite) Dial() (err error) {
	r.Log.Info("Dialing remote kite: [%s %s]", r.Kite.Name, r.Kite.URL.String())
	return r.client.Dial(r.Kite.URL.String())
}

// Dial connects to the remote Kite. If it can't connect, it retries indefinitely.
func (r *RemoteKite) DialForever() error {
	r.Log.Info("Dialing remote kite: [%s %s]", r.Kite.Name, r.Kite.URL.String())
	return r.client.DialForever(r.Kite.URL.String())
}

func (r *RemoteKite) Close() {
	r.client.Close()
}

// OnConnect registers a function to run on connect.
func (r *RemoteKite) OnConnect(handler func()) {
	r.client.OnConnect(handler)
}

// OnDisconnect registers a function to run on disconnect.
func (r *RemoteKite) OnDisconnect(handler func()) {
	r.client.OnDisconnect(handler)
}

func (r *RemoteKite) tokenRenewer() {
	for {
		// Token will be renewed before it expires.
		renewTime := r.Authentication.validUntil.Add(-30 * time.Second)
		select {
		case <-time.After(renewTime.Sub(time.Now().UTC())):
			if err := r.renewTokenUntilDisconnect(); err != nil {
				return
			}
		case <-r.disconnect:
			return
		}
	}
}

// renewToken retries until the request is successful or disconnect.
func (r *RemoteKite) renewTokenUntilDisconnect() error {
	const retryInterval = 10 * time.Second

	if err := r.renewToken(); err == nil {
		return nil
	}

loop:
	for {
		select {
		case <-time.After(retryInterval):
			if err := r.renewToken(); err != nil {
				r.Log.Error("error: %s Cannot renew token for Kite: %s I will retry in %d seconds...", err.Error(), r.Kite.ID, retryInterval)
				continue
			}

			break loop
		case <-r.disconnect:
			return errors.New("disconnect")
		}
	}

	return nil
}

func (r *RemoteKite) renewToken() error {
	tokenString, err := r.localKite.Kontrol.GetToken(&r.Kite)
	if err != nil {
		return err
	}

	token, err := jwt.Parse(tokenString, r.localKite.getRSAKey)
	if err != nil {
		return errors.New("Cannot parse token")
	}

	exp := time.Unix(int64(token.Claims["exp"].(float64)), 0).UTC()

	r.Authentication.Key = tokenString
	r.Authentication.validUntil = &exp

	return nil
}

// getRSAKey returns the corresponding public key for the issuer of the token.
// It is called by jwt-go package when validating the signature in the token.
func (k *Kite) getRSAKey(token *jwt.Token) ([]byte, error) {
	issuer, ok := token.Claims["iss"].(string)
	if !ok {
		return nil, errors.New("Token does not contain a valid issuer claim")
	}

	key, ok := k.trustedKontrolKeys[issuer]
	if !ok {
		return nil, fmt.Errorf("Issuer is not trusted: %s", issuer)
	}

	return key, nil
}

// callOptions is the type of first argument in the dnode message.
// Second argument is a callback function.
// It is used when unmarshalling a dnode message.
type callOptions struct {
	// Arguments to the method
	Kite             protocol.Kite   `json:"kite"`
	Authentication   Authentication  `json:"authentication"`
	WithArgs         dnode.Arguments `json:"withArgs" dnode:"-"`
	ResponseCallback dnode.Function  `json:"responseCallback" dnode:"-"`
}

// callOptionsOut is the same structure with callOptions.
// It is used when marshalling a dnode message.
type callOptionsOut struct {
	callOptions

	// Override this when sending because args will not be a *dnode.Partial.
	WithArgs []interface{} `json:"withArgs"`

	// Override for sending. Incoming type is dnode.Function.
	ResponseCallback Callback `json:"responseCallback"`
}

// That's what we send as a first argument in dnode message.
func wrapMethodArgs(args []interface{}, tr dnode.Transport) []interface{} {
	r := tr.Properties()["remoteKite"].(*RemoteKite)

	responseCallback := args[len(args)-1].(Callback) // last item
	args = args[:len(args)-1]                        // previous items

	options := callOptionsOut{
		WithArgs:         args,
		ResponseCallback: responseCallback,
		callOptions: callOptions{
			Kite:           r.localKite.Kite,
			Authentication: r.Authentication,
		},
	}

	return []interface{}{options}
}

// Authentication is used when connecting a RemoteKite.
type Authentication struct {
	// Type can be "kodingKey", "token" or "sessionID" for now.
	Type       string     `json:"type"`
	Key        string     `json:"key"`
	validUntil *time.Time `json:"-"`
}

// response is the type of the return value of Tell() and Go() methods.
type response struct {
	Result *dnode.Partial
	Err    error
}

// Tell makes a blocking method call to the server.
// Waits until the callback function is called by the other side and
// returns the result and the error.
func (r *RemoteKite) Tell(method string, args ...interface{}) (result *dnode.Partial, err error) {
	return r.TellWithTimeout(method, 0, args...)
}

// TellWithTimeout does the same thing with Tell() method except it takes an
// extra argument that is the timeout for waiting reply from the remote Kite.
// If timeout is given 0, the behavior is same as Tell().
func (r *RemoteKite) TellWithTimeout(method string, timeout time.Duration, args ...interface{}) (result *dnode.Partial, err error) {
	response := <-r.GoWithTimeout(method, timeout, args...)
	return response.Result, response.Err
}

// Go makes an unblocking method call to the server.
// It returns a channel that the caller can wait on it to get the response.
func (r *RemoteKite) Go(method string, args ...interface{}) chan *response {
	return r.GoWithTimeout(method, 0, args...)
}

// GoWithTimeout does the same thing with Go() method except it takes an
// extra argument that is the timeout for waiting reply from the remote Kite.
// If timeout is given 0, the behavior is same as Go().
func (r *RemoteKite) GoWithTimeout(method string, timeout time.Duration, args ...interface{}) chan *response {
	// We will return this channel to the caller.
	// It can wait on this channel to get the response.
	r.Log.Debug("Telling method [%s] on kite [%s]", method, r.Name)
	responseChan := make(chan *response, 1)

	r.send(method, args, timeout, responseChan)

	return responseChan
}

// send sends the method with callback to the server.
func (r *RemoteKite) send(method string, args []interface{}, timeout time.Duration, responseChan chan *response) {
	// To clean the sent callback after response is received.
	// Send/Receive in a channel to prevent race condition because
	// the callback is run in a separate goroutine.
	removeCallback := make(chan uint64, 1)

	// When a callback is called it will send the response to this channel.
	doneChan := make(chan *response, 1)

	cb := r.makeResponseCallback(doneChan, removeCallback)
	args = append(args, cb)

	// BUG: This sometimes does not return an error, even if the remote
	// kite is disconnected. I could not find out why.
	// Timeout below in goroutine saves us in this case.
	callbacks, err := r.client.Call(method, args...)
	if err != nil {
		responseChan <- &response{
			Result: nil,
			Err:    &Error{"sendError", err.Error()},
		}
		return
	}

	// Use default timeout from r (RemoteKite) if zero.
	if timeout == 0 {
		timeout = r.tellTimeout
	}

	// Waits until the response has came or the connection has disconnected.
	go func() {
		select {
		case resp := <-doneChan:
			responseChan <- resp
		case <-r.disconnect:
			responseChan <- &response{nil, &Error{"disconnect", "Remote kite has disconnected"}}
		case <-time.After(timeout):
			responseChan <- &response{nil, &Error{"timeout", "Did not get the response in allowed time"}}

			// Remove the callback function from the map so we do not
			// consume memory for unused callbacks.
			if id, ok := <-removeCallback; ok {
				r.client.RemoveCallback(id)
			}
		}
	}()

	sendCallbackID(callbacks, removeCallback)
}

// sendCallbackID send the callback number to be deleted after response is received.
func sendCallbackID(callbacks map[string]dnode.Path, ch chan uint64) {
	// TODO now, it is not the max id that is response callback.
	if len(callbacks) > 0 {
		// Find max callback ID.
		max := uint64(0)
		for id, _ := range callbacks {
			i, _ := strconv.ParseUint(id, 10, 64)
			if i > max {
				max = i
			}
		}

		ch <- max
	} else {
		close(ch)
	}
}

// makeResponseCallback prepares and returns a callback function sent to the server.
// The caller of the Tell() is blocked until the server calls this callback function.
// Sets theResponse and notifies the caller by sending to done channel.
func (r *RemoteKite) makeResponseCallback(doneChan chan *response, removeCallback <-chan uint64) Callback {
	return Callback(func(request *Request) {
		// Single argument of response callback.
		var resp struct {
			Result *dnode.Partial `json:"result"`
			Err    *Error         `json:"error"`
		}

		// Notify that the callback is finished.
		defer func() {
			if resp.Err != nil {
				r.Log.Warning("Error received from remote Kite: %s", resp.Err.Error())
				doneChan <- &response{resp.Result, resp.Err}
			} else {
				doneChan <- &response{resp.Result, nil}
			}
		}()

		// Remove the callback function from the map so we do not
		// consume memory for unused callbacks.
		if id, ok := <-removeCallback; ok {
			r.client.RemoveCallback(id)
		}

		// We must only get one argument for response callback.
		arg, err := request.Args.SliceOfLength(1)
		if err != nil {
			resp.Err = &Error{Type: "invalidResponse", Message: err.Error()}
			return
		}

		// Unmarshal callback response argument.
		err = arg[0].Unmarshal(&resp)
		if err != nil {
			resp.Err = &Error{Type: "invalidResponse", Message: err.Error()}
			return
		}

		// At least result or error must be sent.
		keys := make(map[string]interface{})
		err = arg[0].Unmarshal(&keys)
		_, ok1 := keys["result"]
		_, ok2 := keys["error"]
		if !ok1 && !ok2 {
			resp.Err = &Error{
				Type:    "invalidResponse",
				Message: "Server has sent invalid response arguments",
			}
			return
		}
	})
}
