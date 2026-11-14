package scraper

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Chain configures an endpoint to run immediately after another endpoint
// (its "upstream"). Each result produced by the upstream becomes one call
// of this endpoint: the upstream's result fields feed this endpoint's
// {{param}} templates in its URL, Body and Headers.
type Chain struct {
	// From is the config path of the upstream endpoint.
	From string `json:"from"`
	// Param limits the passed data to a single upstream result field.
	// When empty, every field of each upstream result is passed through.
	Param string `json:"param,omitempty"`
}

// UpstreamError is returned when a remote endpoint cannot be scraped:
// the HTTP call failed or the response was a non-2xx status.
type UpstreamError struct {
	Endpoint string `json:"endpoint"`
	Status   int    `json:"status,omitempty"`
	Detail   string `json:"detail"`
	// Err holds the underlying transport error, if any.
	Err error `json:"-"`
}

func (e *UpstreamError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("upstream %q failed: HTTP %d: %s", e.Endpoint, e.Status, e.Detail)
	}
	return fmt.Sprintf("upstream %q failed: %s", e.Endpoint, e.Detail)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// HTTPStatus maps an upstream failure onto the status served by this
// scraper to its own callers.
func (e *UpstreamError) HTTPStatus() int { return 502 }

// ChainError is the stable error structure returned by a chained
// endpoint when it cannot produce results. It never degrades into an
// empty list: callers always receive this object instead.
type ChainError struct {
	Message  string      `json:"-"`
	Endpoint string      `json:"endpoint"`
	Upstream *ChainError `json:"upstream,omitempty"`
	// Status is the HTTP status of the failing upstream hop, when known.
	Status int `json:"status,omitempty"`
}

// MarshalJSON keeps the historical leading "error" key while exposing
// the chain-specific fields.
func (e *ChainError) MarshalJSON() ([]byte, error) {
	type alias ChainError
	return json.Marshal(struct {
		Error string `json:"error"`
		alias
	}{
		Error: e.Message,
		alias: alias(*e),
	})
}

// UnmarshalJSON mirrors MarshalJSON so callers can decode the stable
// error structure back into a ChainError.
func (e *ChainError) UnmarshalJSON(data []byte) error {
	type alias ChainError
	aux := struct {
		Error string `json:"error"`
		alias
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*e = ChainError(aux.alias)
	e.Message = aux.Error
	return nil
}

func (e *ChainError) Error() string {
	if e.Upstream != nil {
		return fmt.Sprintf("chain endpoint %q failed: %v", e.Endpoint, e.Upstream)
	}
	return fmt.Sprintf("chain endpoint %q failed: %s", e.Endpoint, e.Message)
}

// HTTPStatus mirrors the upstream hop's mapping (502 for upstream
// failures, 500 for local template/extraction errors).
func (e *ChainError) HTTPStatus() int {
	if e.Status != 0 {
		return 502
	}
	return 500
}

// runChain executes endpoint e with its upstream. The upstream is run
// first; each of its results fans out into one downstream call.
func (e *Endpoint) runChain(params map[string]any) ([]Result, error) {
	if e.registry == nil || e.Chain == nil || e.Chain.From == "" {
		return nil, fmt.Errorf("endpoint %q is not linked to an upstream endpoint", e.Name)
	}
	if err := e.checkChainCycle(map[string]bool{}); err != nil {
		return nil, err
	}
	upstream := e.registry[e.Chain.From]
	if upstream == nil {
		// LoadConfig validates this; kept as a runtime safety net.
		return nil, e.wrapChainError(fmt.Errorf("upstream endpoint %q is not configured", e.Chain.From), 0)
	}
	results, err := upstream.executeAny(params)
	if err != nil {
		// Attribute the failure to the hop that produced it before
		// nesting it, so every link in the chain is preserved.
		return nil, e.wrapChainError(upstream.asChainError(err), upstreamStatus(err))
	}
	// An empty upstream means there is nothing to fan out — a legitimate
	// empty result, not an upstream failure.
	out := make([]Result, 0)
	for _, item := range results {
		nextParams := chainParams(params, item, e.Chain.Param)
		part, err := e.executeOnce(nextParams)
		if err != nil {
			// the failure is this endpoint's own hop; wrapChainError
			// attributes it to e without an extra layer.
			return nil, e.wrapChainError(err, upstreamStatus(err))
		}
		out = append(out, part...)
	}
	return out, nil
}

// asChainError attributes a failure originating from an upstream
// endpoint's own HTTP call to that endpoint, so every hop in a chain
// is represented as a ChainError that the next hop can nest.
func (upstream *Endpoint) asChainError(err error) error {
	var ce *ChainError
	if errors.As(err, &ce) {
		return err
	}
	return &ChainError{
		Message:  err.Error(),
		Endpoint: upstream.Name,
		Status:   upstreamStatus(err),
	}
}

// checkChainCycle walks the Chain.From links and rejects any loop.
func (e *Endpoint) checkChainCycle(seen map[string]bool) error {
	name := e.Name
	if seen[name] {
		return fmt.Errorf("chain cycle detected at endpoint %q", name)
	}
	seen[name] = true
	if e.Chain == nil || e.Chain.From == "" || e.registry == nil {
		return nil
	}
	next := e.registry[e.Chain.From]
	if next == nil {
		return fmt.Errorf("endpoint %q references unknown upstream endpoint %q", name, e.Chain.From)
	}
	return next.checkChainCycle(seen)
}

// chainParams merges the caller-supplied params with one upstream
// result. Upstream values win on key collision so a chain cannot
// silently request with stale caller data. In Param mode, a field
// missing from the upstream result is marked with the absent sentinel
// so template rendering reports "missing param" rather than emitting
// an empty string.
func chainParams(base map[string]any, item Result, param string) map[string]any {
	out := make(map[string]any, len(base)+len(item)+1)
	for k, v := range base {
		out[k] = v
	}
	if param != "" {
		if v, ok := item[param]; ok {
			out[param] = v
		} else {
			out[param] = missingParamSentinel
		}
		return out
	}
	for k, v := range item {
		out[k] = v
	}
	return out
}

// wrapChainError normalises every failure inside a chain into the
// stable ChainError structure, nesting the upstream hop's error when
// the failure originated further up the chain.
func (e *Endpoint) wrapChainError(err error, status int) error {
	ce := &ChainError{
		Message:  err.Error(),
		Endpoint: e.Name,
		Status:   status,
	}
	var nested *ChainError
	if errors.As(err, &nested) {
		ce.Upstream = nested
	}
	return ce
}

// httpStatusError lets typed errors choose the HTTP status served to
// callers (e.g. 502 for upstream failures).
type httpStatusError interface {
	HTTPStatus() int
}

// writeError renders err as the scraper's stable JSON error envelope.
// Structured chain/upstream errors serialise with their full shape;
// plain errors keep the historical {"error": "..."} form.
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var hse httpStatusError
	if errors.As(err, &hse) {
		status = hse.HTTPStatus()
	}
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if jerr := enc.Encode(err); jerr != nil {
		// fall back to the plain historical envelope
		w.Write(jsonerr(err))
	}
}

// upstreamStatus extracts a known upstream HTTP status from an error.
func upstreamStatus(err error) int {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Status
	}
	var ce *ChainError
	if errors.As(err, &ce) {
		return ce.Status
	}
	return 0
}
