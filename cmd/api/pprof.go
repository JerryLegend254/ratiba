package main

import (
	"net/http"
	"net/http/pprof"
)

// registerPprof mounts the Go profiling handlers on a caller-supplied mux.
//
// Importing net/http/pprof for its side effect would register these handlers on
// http.DefaultServeMux at package init, where they are one accidental
// `http.ListenAndServe(addr, nil)` away from being public. Mounting them
// explicitly keeps them confined to the private listener in main.go, which is
// itself refused in production.
func registerPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}
