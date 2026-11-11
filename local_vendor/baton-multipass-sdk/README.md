# baton-multipass-sdk (temporary local copy)

Temporary, trimmed copy of `github.com/conductorone/baton-multipass-sdk`
(`pkg/multipass`) at v0.0.1, commit b9f4b677. The upstream module is private;
this copy exists until it is public (IGA-5320).

It is an ordinary package of this connector's module, imported as
`github.com/conductorone/baton-grafana/local_vendor/baton-multipass-sdk/pkg/multipass`.
There is no nested `go.mod`, no `require` and no `replace`.

Do not edit this code to diverge from upstream.

To switch to the public module: add it to the root `go.mod`, change the import
path, delete this folder, then run `go mod tidy && go mod vendor`.
