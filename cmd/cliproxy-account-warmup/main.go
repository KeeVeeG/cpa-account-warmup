// Command cliproxy-account-warmup builds a native CLIProxyAPI plugin.
package main

/*
#include "bridge.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"sync"
	"unsafe"

	"cliproxy-account-warmup/internal/plugin"
)

const maxRPCBytes = 16 << 20

var lifecycle sync.Mutex
var bridge sync.RWMutex
var calls = newRPCGate()

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil || host.abi_version != 1 || host.call == nil || host.free_buffer == nil {
		return 1
	}
	lifecycle.Lock()
	defer lifecycle.Unlock()
	calls.close()
	bridge.Lock()
	C.warmup_set_host(host)
	bridge.Unlock()
	calls.open(plugin.New(nativeHost{}))
	api.abi_version = 1
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (code C.int) {
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	defer func() {
		if recover() != nil {
			writeResponse(response, plugin.ErrorEnvelope("internal_error", "The plugin could not complete this request."))
			code = 1
		}
	}()
	if method == nil || uint64(requestLen) > maxRPCBytes || (requestLen > 0 && request == nil) {
		writeResponse(response, plugin.ErrorEnvelope("invalid_request", "Invalid plugin RPC request."))
		return 1
	}
	methodName := C.GoString(method)
	handler := calls.acquire(methodName)
	if handler == nil {
		writeResponse(response, plugin.ErrorEnvelope("not_initialized", "The plugin is not initialized."))
		return 1
	}
	defer calls.release()
	var raw []byte
	if requestLen > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	if !writeResponse(response, handler.Handle(methodName, raw)) {
		return 1
	}
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	lifecycle.Lock()
	defer lifecycle.Unlock()
	calls.close()
	bridge.Lock()
	defer bridge.Unlock()
	C.warmup_set_host(nil)
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) bool {
	if len(raw) == 0 || len(raw) > maxRPCBytes {
		return false
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return false
	}
	response.ptr, response.len = ptr, C.size_t(len(raw))
	return true
}

type nativeHost struct{}

func (nativeHost) Call(method string, request any, target any) error {
	bridge.RLock()
	defer bridge.RUnlock()
	raw, err := json.Marshal(request)
	if err != nil {
		return errors.New("Could not encode host request.")
	}
	if len(raw) > maxRPCBytes {
		return errors.New("Host request is too large.")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	cRequest := C.CBytes(raw)
	if cRequest == nil {
		return errors.New("Could not allocate host request.")
	}
	defer C.free(cRequest)
	var response C.cliproxy_buffer
	code := C.warmup_host_call(cMethod, (*C.uint8_t)(cRequest), C.size_t(len(raw)), &response)
	if response.ptr != nil {
		defer C.warmup_host_free(response.ptr, response.len)
	}
	if response.ptr == nil || response.len == 0 || uint64(response.len) > maxRPCBytes {
		return errors.New("Invalid host response.")
	}
	responseBytes := C.GoBytes(response.ptr, C.int(response.len))
	return decodeHostResult(responseBytes, int(code), target)
}

type hostCallError struct{ statusCode int }

func (e hostCallError) Error() string {
	return "Host callback failed; upstream completion may be unknown."
}

func (e hostCallError) StatusCode() int { return e.statusCode }

func decodeHostResult(raw []byte, code int, target any) error {
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			HTTPStatus int `json:"http_status"`
		} `json:"error"`
	}
	// Host error strings can contain sensitive upstream content. They are neither
	// returned to the browser nor written to logs.
	if err := json.Unmarshal(raw, &env); err != nil {
		return hostCallError{}
	}
	if code != 0 || !env.OK {
		status := 0
		if env.Error != nil && env.Error.HTTPStatus >= 400 && env.Error.HTTPStatus <= 599 {
			status = env.Error.HTTPStatus
		}
		return hostCallError{statusCode: status}
	}
	if err := json.Unmarshal(env.Result, target); err != nil {
		return errors.New("Invalid host result.")
	}
	return nil
}
