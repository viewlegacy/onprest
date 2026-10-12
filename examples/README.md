# Examples

Each example has its own configuration and a README with the commands. They use public demonstration credentials, so run them only on your own machine.

The `csv`, `file-output`, `file-input`, and `http` examples need only the released `onprest-gateway` and `onprest-agent` binaries and the files in their directory (plus Python for the stand-in API in `http`). The `sdk` and `wasm` examples build a module with Go from a clone of this repository, and `mutation-reconciliation` runs from a clone with Docker and make.

| Example | Executor | What it shows |
|---|---|---|
| Files in this directory | `db` | The [Quick Start](https://docs.onprest.viewlegacy.com/quick-start): read and update a PostgreSQL table through REST and MCP |
| [csv](csv/) | `csv` | Filter a CSV from a folder or an upload and download the selected rows |
| [file-output](file-output/) | `file` | Download a completed report from a folder |
| [file-input](file-input/) | `file` | Upload a PDF and save it into a folder |
| [http](http/) | `http` | Call an internal API with validated arguments and publish part of the response |
| [sdk](sdk/) | `wasm` | Write a capability in Go, test it locally, and call it through the gateway |
| [mutation-reconciliation](mutation-reconciliation/) | `db` | Confirm an insert after losing its response, on PostgreSQL, MySQL, SQL Server, or Oracle |
| [wasm](wasm/) | `wasm` | A module that uses the raw host interface without the SDK |
