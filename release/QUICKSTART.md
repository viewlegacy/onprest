# Onprest Quick Start files

These files contain public development credentials and a disposable PostgreSQL
setup. Use them only for local evaluation on a trusted computer. Never copy
these keys, API credentials, or database password into a real deployment.

Download the matching `onprest-X.Y.Z-<os>-<arch>` binary archive from the same
GitHub Release. Extract both archives in a working directory. No source
checkout, Go toolchain, or `make` is needed. Docker Compose is needed only if
you use the included disposable database.

Follow the full Quick Start at:
<https://docs.onprest.viewlegacy.com/quick-start>

The `postgres-init.sql` file initializes an empty `legacy` database and creates
the example DB role; it requires an administrative PostgreSQL account. For an
existing PostgreSQL instance, use a disposable database and adjust the
connection values in `capability.postgres.yaml`.
