# Working on PAI

- `cmd/pai` is the worker, TUI and CLI; `cmd/pai-server` is the server. `pkg/harness` is the agent loop, `pkg/memory` the memory store, `pkg/server` the HTTP surface, `web/` the web app.
- Go rules: one owner per fact; a failure the model should read is a result, one the server should handle is an error, never both; constants at the top of the file with the reason for the value.
- Session files are append only and the source of truth. Never rewrite or delete records; add a record kind instead.
- Memory never deletes: invalidate with a reason, and make merges and edits new versions that name what they replace.
- Run `go vet ./...` and `go test ./...` before a commit. After changing `web/src`, run `npm run build` in `web/` and commit `web/dist`: the server embeds it, and CI checks it is current.
- `working_dir/` is gitignored: build there.
