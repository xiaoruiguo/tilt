package starkit

import "go.starlark.net/starlark"

// An extension to a starlark execution environment.
type Extension interface {
	// Called when execution begins.
	OnStart(e *Environment)

	// Called before each new Starlark file is loaded
	OnExec(path string)

	// Called before each builtin is called
	OnBuiltinCall(name string, fn *starlark.Builtin)
}

// Embed this struct to get default values for the Extension interface.
type DefaultExtension struct {
}

func (DefaultExtension) OnStart(e *Environment)                          {}
func (DefaultExtension) OnExec(path string)                              {}
func (DefaultExtension) OnBuiltinCall(name string, fn *starlark.Builtin) {}
