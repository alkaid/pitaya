# Stateful owner assignment operations

`pitaya-ownerctl` inventories and removes retired stateful-owner assignments.
It is an on-demand command, not a controller. It never changes registrations
and never deletes a live assignment.

Build it from the repository with:

```sh
make build-ownerctl
```

## Connection flags

Every operation accepts `--endpoints`, `--prefix`, `--user`, `--password`,
`--dial-timeout`, and `--timeout`. TLS uses `--tls`, `--ca-file`,
`--cert-file`, `--key-file`, and `--server-name`. The equivalent environment
variables use the `PITAYA_OWNERCTL_` prefix, for example
`PITAYA_OWNERCTL_ENDPOINTS` and `PITAYA_OWNERCTL_CA_FILE`.

The namespace prefix must match `pitaya.cluster.sd.etcd.prefix` and end in `/`.
The default is `pitaya/`. Passwords and private-key contents are never included
in reports.

## Inventory

```sh
pitaya-ownerctl inventory --format json > owner-inventory.json
```

Each assignment is classified as:

- `live`: the registration exists with the creation revision recorded as the
  owner epoch.
- `stale`: the registration is missing or belongs to a replacement generation.
- `corrupt`: the key or strict assignment record is invalid. Corrupt records
  are reported and are never automatically deleted.

The JSON report includes the full assignment and registration keys, revisions,
a registration value digest, and low-cardinality counts/latency grouped only by
server type, status, and cleanup result. `--limit` bounds the operation; reaching
the limit fails instead of silently returning a partial cleanup inventory.

## Prune stale assignments

Both prune commands are dry-run unless `--apply` is present:

```sh
pitaya-ownerctl prune-stale --format json
pitaya-ownerctl prune-stale --apply --format json > owner-prune-result.json
```

Before the first delete, the command validates every selected key and record.
Apply then compares the assignment revision and the stale registration evidence
in one etcd transaction. A registration that appears, changes generation, or
changes value after inventory produces `conflict`; it is not deleted. The next
valid resolve can recreate a deleted stale assignment.

For a business-reviewed retirement list, provide one complete etcd assignment
key per line. Blank lines and `#` comments are ignored:

```text
pitaya/stateful-owners/v1/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

```sh
pitaya-ownerctl prune-list --input retired-assignments.txt --format json
pitaya-ownerctl prune-list --input retired-assignments.txt --apply --format json
```

If a reviewed assignment is still live, drain that owner through the normal
application procedure and wait for its registration generation to disappear or
be replaced. Run inventory again, review the new evidence, and only then apply.
There is no live-owner override.

Keep the before and after JSON reports with the operational change record.
If an apply stops after a partial etcd failure, the command writes the completed
cleanup entries and an `error` field before exiting non-zero.
