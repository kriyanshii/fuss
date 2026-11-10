# rqlite (planned)

rqlite is planned. An example adapter exists (`rqlite_adapter.go`). Its check
is not yet backed by a documented promise.

There is no supported run command yet. Integration tests that need an
`rqlited` binary:

```bash
go test -run TestRqliteIntegration
```
