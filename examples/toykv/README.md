# toykv

A tiny Go key-value server (stdlib only) used as a Fuss demo target.

```
toykv -data-dir <dir> [-addr 127.0.0.1:18731] [-fsync]
```

- `PUT /k/<key>` with a body stores that value
- `GET /k/<key>` returns the value or 404

## Promise

A PUT that returns 200 survives a kill and restart.

## Bug

Without `-fsync` (the default) the server returns 200 as soon as the key is
in memory. The store file under `-data-dir` is flushed every 200ms. A kill
before that flush drops acked keys.

`-fsync` writes and syncs the store before PUT returns 200.
`toykv-fsync.yaml` passes that flag. `toykv.yaml` does not.

## Limits

Fuss does not write into `-data-dir`. Acks are recorded outside it. After
restart, the check only `GET`s each acknowledged key. It does not read the
store file.
