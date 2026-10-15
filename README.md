[![HivePanel](https://hivepanel.dev/assets/imgs/HivePanelLogo.png)](https://hivepanel.dev)

![Discord](https://img.shields.io/discord/1522542806831206462?label=Discord&logo=Discord&logoColor=white)

# HivePanel Worker

HivePanel Worker is the node agent responsible for installing, running and managing **Cells** in the HivePanel ecosystem.

HivePanel handles users, permissions and administration. Worker runs on each node and provides the runtime, console, file access, backups and resource measurements used by the Panel.

## Features

- **UUID-based Cells** with persistent definitions and runtime recovery.
- **Combs** for game/application templates, Docker images, startup commands, variables and installation steps.
- **Docker and process runtimes** behind a shared runtime interface.
- **Installation engine** with progress, captured installer output and retry/reinstall support.
- **Live console** with command submission, WebSocket streaming and short-lived console sessions.
- **Resource metrics** for Cells and nodes, including CPU, memory, disk and network measurements where supported by the runtime.
- **File management** with editing, uploads, downloads, URL imports, archives, extraction and restore operations.
- **SFTP access** with Panel-authenticated credentials, filesystem confinement and permission checks.
- **Backups** with download, restore, file browsing, extraction and mounted-backup access.
- **Allocation management** with primary/additional allocations and checks that prevent removal of allocations still in use.
- **File APIs for HivePanel’s game-aware configuration editor**, with supported files defined by Combs in the Panel.
- **Minecraft player controls** for listing players, kick, ban, op and deop.
- **Server imports and migration source detection**.
- **Cell locks** that can disable power, files, settings, backups and importer actions.
- **Panel registration and heartbeat reporting**.
- **Worker update requests and update-status reporting**.
- **Authenticated REST API** for Panel-to-node operations.

Scheduled tasks are managed by HivePanel and use Worker operations to execute their actions. Swarm clustering is not implemented in this Worker.

## Minecraft content installation

Worker integrates with HiveRegistry to install Minecraft Java content:

- Vanilla, Paper, Fabric, Forge and NeoForge server software.
- Mods and plugins resolved through HiveRegistry.
- Custom packs built in HivePanel.
- Published Modrinth and CurseForge packs with supported manifests.
- Required dependencies, pinned releases and server overrides supplied by the Registry plan.

HiveRegistry holds provider credentials and performs catalogue lookup, dependency resolution and compatibility checks. Worker downloads Registry artifacts, verifies their hashes and applies the installation plan. Modrinth and CurseForge API keys are not needed on the node.

Content installs require an installed, unlocked and stopped Cell. Files are downloaded into staging before replacement. Replaced files are retained beneath:

```text
<cell-directory>/.hivepanel/content-backups/<transaction>/original/
```

Full pack installs replace the existing `mods` directory. Individual mod/plugin installs replace files with matching names. Overrides can replace existing configuration or other files with matching paths, with originals retained.

Installation locks prevent conflicting power, configuration, file and reinstall actions. Existing SFTP writers also honor newly applied locks. Transaction journals recover interrupted file changes, and completed runtime installs synchronize their startup command back to HivePanel.

Forge and NeoForge installers run inside the Cell's existing Java Docker image. Use an image appropriate to the selected Minecraft/loader version. Compatibility metadata cannot guarantee that a custom pack will launch successfully.

Bedrock, Quilt installation, Spigot BuildTools and arbitrary pack installer scripts are not supported by this content installer. Some packs require an author's dedicated server distribution rather than a standard Modrinth/CurseForge manifest.

## Requirements

- A Linux node for the current managed Cell identity and file-ownership implementation.
- Docker installed and running when using the Docker runtime or Forge/NeoForge content installation.
- A compatible HivePanel installation reachable from the Worker.
- Writable directories for definitions, Cell files, backups and backup mounts.
- Go **1.26 or newer** when building this source revision.
- Access to required image registries and download sources. HiveRegistry content installation requires a reachable HTTPS Registry origin.

The source contains platform-specific helpers, but managed Cell identity operations currently require Linux. Compiling the binary on another platform does not make those operations supported.

## Build and run

From the Worker repository:

```bash
go build -o hiveworker ./cmd/daemon
./hiveworker --config /etc/hivepanel/worker.yml
```

A starter configuration is provided in `config.yaml.example`. Merge its settings into your node configuration and use the registration details generated by HivePanel.

On Linux, the default configuration path is `/etc/hivepanel/worker.yml`. Override it with `--config` when required. The binary needs permission to manage its configured directories, game identity and runtime.

For production, run Worker under the service installed by your deployment tooling. A typical installation uses `hiveworker.service`:

```bash
systemctl status hiveworker --no-pager -l
journalctl -u hiveworker -n 100 --no-pager
```

## Configuration

Example configuration with placeholder registration details:

```yaml
panel:
  url: "https://panel.example.com"

worker:
  registration_token: "hpreg_your-registration-token"
  token: ""
  listen: "0.0.0.0:8080"
  ssl:
    enabled: false
    cert: ""
    key: ""

sftp:
  enabled: true
  listen: "0.0.0.0:2022"
  public_fqdn: "node.example.com"
  public_port: 2022
  host_key_path: "/etc/hivepanel/keys/sftp_host_ed25519"
  auth_timeout_seconds: 10

node:
  id: ""

paths:
  data: "/var/lib/hivepanel/data"
  instances: "/var/lib/hivepanel/cells"
  backups: "/var/lib/hivepanel/backups"
  backup_mounts: "/var/lib/hivepanel/backup_mounts"

runtime:
  type: "docker"

docker:
  network: "hivepanel"

minecraft:
  registry_url: "https://registry.hivepanel.dev"

allocations:
  entries:
    - ip: "0.0.0.0"
      port: 25565
    - ip: "0.0.0.0"
      port: 25566
```

| Section | Purpose |
| --- | --- |
| `panel` | Panel URL used for registration, heartbeats and authentication callbacks. |
| `worker` | API bind address, registration/API credentials and TLS settings. |
| `sftp` | SFTP bind address, advertised hostname/port and persistent SSH host key. |
| `node` | Node identity associated with this Worker. |
| `paths` | Definition/state storage, Cell instances, backups and backup mounts. |
| `runtime` | Runtime selection: `docker` or `process`. |
| `docker` | Docker network selection. |
| `minecraft` | HTTPS HiveRegistry origin for content downloads; defaults to `https://registry.hivepanel.dev` if omitted. |
| `allocations` | IP/port allocations available on this node. |

Preserve the credentials and node ID assigned during registration. Restart Worker after manually changing its configuration.

A wildcard allocation such as `0.0.0.0:25565` is a bind address, not a client connection address. Players connect using the node's reachable hostname or IP.

## Connectivity

Worker communicates with several services in different directions:

| Connection | Use |
| --- | --- |
| HivePanel → Worker API | Cell management, file actions, metrics, backups and installation requests. |
| Worker → HivePanel | Registration, heartbeat reporting and SFTP authentication. |
| Browser → Worker WebSocket | Live console sessions when connected directly through the configured node address. |
| SFTP client → Worker | File transfers on the configured SFTP port. |
| Worker → Docker/image registries | Container management and image retrieval. |
| Worker → HiveRegistry | Minecraft artifact downloads. |

The Panel and Worker must each be able to resolve and reach the address they use for the other. Configuring an internal node address in the Panel does not change Worker's `panel.url` heartbeat destination.

Heartbeats run every 15 seconds when the Panel URL, Worker token and node ID are configured. Missing values disable heartbeat reporting.

Use HTTPS/WSS for browser-facing node traffic when HivePanel is served over HTTPS. A reverse proxy must support WebSocket upgrades. SFTP is a separate SSH service and is not carried by an ordinary HTTP reverse proxy.

## API authentication

Most Worker HTTP endpoints require the node's Worker API token:

```http
Authorization: Bearer YOUR_WORKER_TOKEN
```

This is the **Worker token**, not a HiveRegistry token or a user's Panel session. Treat it as a privileged node credential; browser console clients should use short-lived console sessions instead.

Exceptions:

- `GET /health` is public.
- `POST /migration/source-detect` is currently registered without Worker-token middleware. Restrict access through the node's network/reverse proxy where appropriate.
- `GET /cells/{id}/ws` validates a Cell-specific console-session token in its `token` query parameter.

Worker Cell IDs refer to the daemon UUID on that node. HivePanel's own Cell UUID can be different.

## Testing the API

For Bash/Linux:

```bash
WORKER_URL='http://127.0.0.1:8080'
read -rsp 'Worker API token: ' WORKER_TOKEN
echo

curl -fsS "$WORKER_URL/health"

curl -fsS "$WORKER_URL/version" \
  -H "Authorization: Bearer $WORKER_TOKEN"

curl -fsS "$WORKER_URL/cells" \
  -H "Authorization: Bearer $WORKER_TOKEN"
```

Replace `WORKER_URL` with the node's HTTPS URL for remote testing. In Postman, choose **Bearer Token** authentication and enter the Worker token.

Use a daemon Cell ID returned by `/cells`:

```bash
CELL_ID='replace-with-daemon-cell-id'

curl -fsS "$WORKER_URL/cells/$CELL_ID/stats" \
  -H "Authorization: Bearer $WORKER_TOKEN"

curl -fsS "$WORKER_URL/cells/$CELL_ID/install/status" \
  -H "Authorization: Bearer $WORKER_TOKEN"

curl -fsS "$WORKER_URL/cells/$CELL_ID/content/status" \
  -H "Authorization: Bearer $WORKER_TOKEN"
```

`/install/status` reports the base Comb installation. `/content/status` reports the separate Minecraft content job, including its state and retained log output.

For bulk dashboard measurements, `/cells/overview?ids=UUID_A,UUID_B` accepts a comma-separated list of daemon IDs. Omit `ids` to request the node's Cells.

Use HivePanel as the normal management client. Destructive endpoints such as deletion, restore and reinstall act on node data; request schemas and permissions should be handled through the corresponding Panel workflow.

## API reference

These paths are relative to the Worker URL. Unless covered by the authentication exceptions above, they require the Worker token. File, backup and runtime handlers also enforce applicable Cell locks.


### Node and service

| Method | Endpoint |
| --- | --- |
| GET | `/health` |
| GET | `/version` |
| GET | `/node/stats` |
| POST | `/update` |
| GET | `/update/status` |
| PATCH | `/configuration/allocations` |
| GET | `/cells` |
| POST | `/cells` |

### Imports and migration

| Method | Endpoint |
| --- | --- |
| POST | `/migration/source-detect` |
| POST | `/cells/{id}/importer/test` |
| POST | `/cells/{id}/importer/start` |
| GET | `/cells/{id}/importer/status` |

### Combs

| Method | Endpoint |
| --- | --- |
| GET | `/combs` |
| GET | `/combs/{id}` |

### Cells, runtime and console

| Method | Endpoint |
| --- | --- |
| GET | `/cells/overview` |
| GET | `/cells/{id}` |
| DELETE | `/cells/{id}` |
| PATCH | `/cells/{id}/definition` |
| POST | `/cells/{id}/install` |
| GET | `/cells/{id}/install/status` |
| POST | `/cells/{id}/reinstall` |
| POST | `/cells/{id}/start` |
| POST | `/cells/{id}/network/tcp` |
| POST | `/cells/{id}/stop` |
| POST | `/cells/{id}/command` |
| GET | `/cells/{id}/console` |
| POST | `/cells/{id}/console-session` |
| GET | `/cells/{id}/stats` |
| GET | `/cells/{id}/ws` |
| POST | `/cells/{id}/lock` |
| POST | `/cells/{id}/unlock` |

### Minecraft content

| Method | Endpoint |
| --- | --- |
| POST | `/cells/{id}/content/install` |
| GET | `/cells/{id}/content/status` |

### Backups

| Method | Endpoint |
| --- | --- |
| POST | `/cells/{id}/backups` |
| GET | `/cells/{id}/backups` |
| POST | `/cells/{id}/backups/{name}/mount` |
| DELETE | `/cells/{id}/backup-mounts/{mountId}` |
| GET | `/cells/{id}/backup-mounts/{mountId}/files` |
| POST | `/cells/{id}/backup-mounts/{mountId}/restore` |
| GET | `/cells/{id}/backups/files` |
| GET | `/cells/{id}/backups/files/read` |
| POST | `/cells/{id}/backups/files/extract` |
| GET | `/cells/{id}/backups/{backupID}/download` |
| DELETE | `/cells/{id}/backups/{backupID}` |
| POST | `/cells/{id}/backups/{backupID}/restore` |

### Files

| Method | Endpoint |
| --- | --- |
| GET | `/cells/{id}/files` |
| GET | `/cells/{id}/files/read` |
| POST | `/cells/{id}/files/write` |
| DELETE | `/cells/{id}/files/delete` |
| POST | `/cells/{id}/files/folder` |
| POST | `/cells/{id}/files/rename` |
| POST | `/cells/{id}/files/upload` |
| GET | `/cells/{id}/files/download` |
| POST | `/cells/{id}/files/restore` |
| DELETE | `/cells/{id}/files/permanent` |
| POST | `/cells/{id}/files/file` |
| POST | `/cells/{id}/files/upload-url` |
| POST | `/cells/{id}/files/archive` |
| POST | `/cells/{id}/files/extract` |

### Game configuration

| Method | Endpoint |
| --- | --- |
| GET | `/cells/{id}/config` |
| GET | `/cells/{id}/config/read` |
| PATCH | `/cells/{id}/config/write` |

### Minecraft players

| Method | Endpoint |
| --- | --- |
| GET | `/cells/{id}/players` |
| POST | `/cells/{id}/players/kick` |
| POST | `/cells/{id}/players/ban` |
| POST | `/cells/{id}/players/op` |
| POST | `/cells/{id}/players/deop` |

### Console sessions

Create a console session through `POST /cells/{id}/console-session` using the Worker token, then connect to `/cells/{id}/ws?token=CONSOLE_SESSION_TOKEN` over WebSocket. The session token is scoped to that Cell and expires. Do not substitute the long-lived Worker token for a console-session token.

### Minecraft content plans

`POST /cells/{id}/content/install` accepts a resolved HiveRegistry installation plan, not a provider search request. The plan contains pinned artifact URLs, file paths, hashes and runtime information. Worker accepts artifact URLs only from its configured HTTPS Registry origin and does not accept arbitrary download hosts through this endpoint.

Poll `GET /cells/{id}/content/status` for `idle`, `installing`, `complete` or `failed` state and job logs. HivePanel manages preview ownership, user permissions and synchronization of successful startup changes.

### Responses

Successful endpoints return JSON, downloaded file content or a WebSocket stream depending on the route. Error formats vary: many existing handlers return plain text, while Minecraft content handlers return JSON with a `message` field. Clients should handle authentication failures, missing Cells, locks, conflicts and node timeouts rather than assuming every response is JSON.

## Development

Run the test suite:

```bash
go test ./...
```

Run race checks for the content installer and related services:

```bash
go test -race ./internal/minecraft ./internal/cell ./internal/api ./internal/sftp
```

Format Go source:

```bash
go fmt ./...
```

Provider fixture tests live in HiveRegistry. Real Docker image/loader installation should also be checked using a disposable Cell with the correct Java image.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Heartbeats time out | Resolve and reach `panel.url` from the node; check the Panel/reverse proxy and Worker credentials. |
| Panel cannot reach Worker | Check the node address/port configured in Panel, DNS, firewall, TLS and Worker service logs. |
| API returns `401` | Use the node's Worker token; Registry tokens and Panel cookies do not authenticate this API. |
| Console cannot connect | Check WSS/proxy upgrades, browser reachability and the Cell's console-session token. |
| Content installation is rejected | Stop the Cell; check its base installation state, locks and HTTPS Registry configuration. |
| Forge/NeoForge installation fails | Check Docker and the Java version in the Cell's selected image; expand installation logs. |
| An allocation cannot be removed | Check whether an existing Cell still references it. |
| Installation was interrupted | Review the job status, Worker logs and retained content backup before retrying. |
| SFTP login fails | Check Panel connectivity, issued SFTP credentials, user permissions and advertised port. |

## Hive ecosystem

- **HivePanel** — administration, users, permissions and Cell management.
- **HivePanel Worker** — node runtime and operations.
- **HiveRegistry** — Combs, Minecraft catalogue resolution and cached installation assets.

Visit [hivepanel.dev](https://hivepanel.dev) for the project website.


## Generic preinstalls

The content-install endpoint also accepts administrator-offered Registry preinstall plans. It downloads and pins the prebuilt image, creates private configuration inside staging, then commits a full file replacement with an installation backup. Image-download failures leave Cell files intact. Content locks, preview authorization in the Panel, restart recovery and job acknowledgement also apply to these installs. Required credentials are configured through private Cell variables after installation.

Configure the Registry with `registry.url` in `worker.yml`; `minecraft.registry_url` remains a legacy fallback. The Registry must use HTTPS. Preset artifact URLs must match that origin and use `/api/v1/preinstalls/artifacts/{id}`. Linux and Docker are required. The Worker does not automatically start the bot or contact Discord while installing.


Source preinstalls execute administrator-authored `git`, `mkdir`, `copy`, `move`, `delete`, `write_file`, `download`, `extract` (tar.gz) and `run` steps in an isolated temporary Docker container. The only persistent bind is the staging directory; the image filesystem is read-only and host capabilities/socket access are not granted. The runner cleans up after failures and detects interrupted recipe containers during recovery. Secret runtime variables are not forwarded to build steps. Dependency symlinks are accepted only when they resolve inside the staged tree.

Combs may declare a `docker_images` label-to-image map. The Panel validates image choices and passes the selected image through definition updates. Port mappings are recreated on the next stop/start; additional TCP and UDP allocations are included. Cell deletion removes its owned stopped Docker container before deleting Cell files and metadata; Docker removal errors preserve the Cell records and files.

## Node web hosting and reverse proxies

Each Docker node can run one separate Worker-managed Nginx container for Cell domains, HTTPS certificates, WebSockets and dedicated TCP/UDP proxies. Configure it under **Admin → Nodes → Web Proxy**; manage routes under **Cell → Domains & Proxies**. The Worker validates DNS TXT ownership and assigned backend ports before routing. Static, Node.js and PHP-FPM/Nginx Combs use the same domain controls; Minecraft web maps use HTTP routes, while game traffic requires dedicated TCP/UDP ports.

The PHP application image source is in `deploy/web-php/`. Build the offered PHP tags on each applicable node before using the PHP Comb. Proxy settings and certificates persist under `web-proxy` in the configured Worker data directory. A user-defined Docker network and reachable public web/service ports are required.

Authenticated API: `GET/PATCH /configuration/web-proxy`, `DELETE /configuration/web-proxy/routes/{route}`, `GET/POST /cells/{id}/domains`, `POST /cells/{id}/domains/{route}/verify`, `POST /cells/{id}/domains/{route}/certificate`, `DELETE /cells/{id}/domains/{route}`. `{id}` is the Worker daemon ID.
