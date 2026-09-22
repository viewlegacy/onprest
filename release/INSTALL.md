# Install Onprest from a release archive

This archive contains the `onprest-gateway` and `onprest-agent` binaries for the
target named in `RELEASE-MANIFEST.txt`. It does not require a source checkout,
Go, Docker, or a native database client runtime.

Before extraction, verify the checksum and GitHub artifact attestation by
following the public deployment procedure:
<https://docs.onprest.viewlegacy.com/operations/deployment#binary-release-installation>

After extracting a verified archive:

1. Copy `gateway.env.example` to a protected `gateway.env`, and copy
   `capability.yaml.example` to a protected `capability.yaml`.
2. Generate an Agent key pair and a capability-scoped API key:

   ```sh
   ./onprest-gateway create-agent-secret
   ./onprest-gateway create-key --name operator --capabilities get_customer
   ```

   On Windows, use the same commands with `.\onprest-gateway.exe`. Put the
   generated public key in `GATEWAY_AGENT_PUBLIC_KEY`, the private key in
   `gateway.agent_private_key`, and the generated `key_hash` and capability list
   in `GATEWAY_API_KEYS_JSON`. Deliver the plaintext `api_key` to its caller; do
   not put it in `gateway.env`.
3. Replace every example URL, database address, credential, SQL statement, and
   capability contract in `capability.yaml` with deployment-specific values.
4. Validate the completed file against its database before starting the Agent:

   ```sh
   ./onprest-agent validate --config /absolute/path/capability.yaml
   ```

   On Windows, run `onprest-agent.exe validate --config
   C:\absolute\path\capability.yaml` instead.
5. The Gateway reads process environment variables; it does not read
   `gateway.env` itself. On Unix, load the file and start the Gateway:

   ```sh
   set -a
   . ./gateway.env
   set +a
   ./onprest-gateway
   ```

   On Windows PowerShell, set the two configured values in the process or
   service environment, then run `onprest-gateway.exe`:

   ```powershell
   $env:GATEWAY_AGENT_PUBLIC_KEY = '<generated-agent-public-key>'
   $env:GATEWAY_API_KEYS_JSON = '[{"name":"operator","key_hash":"<generated-key-hash>","capabilities":["get_customer"]}]'
   .\onprest-gateway.exe
   ```
6. Start the Agent in another session, or install it with the OS service manager:

   ```sh
   ./onprest-agent --config /absolute/path/capability.yaml
   # Or:
   ./onprest-agent service install --config /absolute/path/capability.yaml
   ./onprest-agent service start
   ```

   Use the `.exe` commands and a Windows path on Windows. Confirm the Gateway
   `/healthz` reports `agent_connected: true` before testing REST or MCP.

The Gateway and Agent must come from the same release. Release acceptance and
provenance are documented at
<https://docs.onprest.viewlegacy.com/operations/release-gate>.
