//go:build js && wasm

// Command verify-wasm is BackupProof's proof verifier compiled to
// WebAssembly, so a browser can check a proof bundle with exactly the code
// `backupproof proof verify` runs, without sending the bundle anywhere.
//
//	GOOS=js GOARCH=wasm go build -o verify.wasm ./cmd/verify-wasm
//
// It defines one function on the page:
//
//	backupproofVerify(bundleJSON, trustedKeys, requireTimestamp) → JSON string
//
// trustedKeys holds bpkey1:… keys, one per line (empty uses the keys inside
// the bundle, with a warning). The result is {"report": …} with the same
// report `backupproof proof verify --json` prints, or {"error": "…"} when the
// input isn't a proof bundle or a key can't be read.
package main

import (
	"syscall/js"
)

// version is set at build time (-X main.version=...).
var version = "dev"

func main() {
	js.Global().Set("backupproofVerify", js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) < 3 {
			return result(nil, "backupproofVerify needs the bundle, the trusted keys and whether a timestamp is required")
		}
		return verify(args[0].String(), args[1].String(), args[2].Truthy())
	}))
	js.Global().Set("backupproofVerifierVersion", version)
	if ready := js.Global().Get("backupproofReady"); ready.Type() == js.TypeFunction {
		ready.Invoke()
	}
	select {} // keep the functions alive
}
