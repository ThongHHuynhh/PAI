# Jetson NVMe deployment assessment

Inspected and consolidated over SSH on 2026-11-05 following user authorization.

## Applied state

The existing `/mnt/nvme/pai-server` is now the complete PAI installation root.
Binaries, worker identity/scratch, Claude cache, private Ollama runtime, models,
temporary directories and application logs are under it. Small boot-readable
systemd hooks, fstab mount entries and compatibility links remain on the OS.
The active services are `pai-server`, `paid`, and `pai-embeddings`.

Ollama 0.40.14 was installed privately, its archive digest verified against the
official release, and embeddinggemma tested on CPU and CUDA 13. The GPU model is
kept resident with startup readiness checking. An in-memory synthetic check
ranked a synonym pair above an unrelated pair. All 85 items were re-embedded to
768 dimensions, preserving their ids, texts and invalidation status; all three
session files remain byte-for-byte unchanged.

The final server uses the **unreleased semantic-retrieval development build**
(`dev`, branch `feat/semantic-retrieval`); the worker/CLI remains v0.2.5. Both
the local admin and Windows workers were verified connected. Temporary private
mount settings were removed from the PAI server/worker during reconnect
troubleshooting; their own TMPDIR remains configured on NVMe. They retain their
normal access to the machine. Pure semantic ranking was checked through the
actual PAI `/memory?q=` API: an existing fact matched with cosine above 0.99 and
no lexical ranks; requests took 82 ms first and 3 ms cached in that small check.
This confirms operation, not general answer accuracy or production throughput.

The remote root contains one `README.md`, deployment metadata, service hook
copies and one intentional rollback snapshot in `backups/20261105T205451Z`.
Download archives, superseded experimental binaries and test artifacts were removed.
The sections below record the original assessment and staged migration plan.

## Existing installation

- NVIDIA Jetson AGX Orin, ARM64, Ubuntu 24.04.4, L4T 39.2.1, driver 595.78,
  reported CUDA compatibility 13.2. Approximately 56 GiB available RAM.
- PAI v0.2.5: `pai-server.service`, running as ABI-Jetson1, and `paid.service`,
  running as root. Server responds successfully on loopback port 8080.
- **Server data is already on NVMe.** `/var/lib/pai-server` is a persistent bind
  mount of `/mnt/nvme/pai-server` from `/dev/nvme0n1p1`. Do not copy it as though
  it were separate data. NVMe has approximately 817 GiB available.
- Binaries `/usr/local/bin/pai` and `/usr/local/bin/pai-server`, worker state
  `/var/lib/pai`, and operator scratch `/home/ABI-Jetson1/.pai` are still on the
  system drive. Scratch occupies approximately 76 MiB.
- 3 session files; 85 memory items, 38 valid, none pinned. All vectors currently
  use `hash-1024`. No embedding config or Ollama installation was found.
- A separate EyeQ container is active. Do not replace its CUDA libraries,
  container runtime, drivers, service, or storage.

## One place for PAI

Use the existing `/mnt/nvme/pai-server` as the installation root. Preserve its
existing config, providers, sessions, memory and mesh paths; add:

```text
/mnt/nvme/pai-server/
  config.json, providers/, sessions/, memory/, mesh/, ...  # existing data
  bin/                        # pai and pai-server executables
  runtime/ollama/              # private Ollama bin/ and lib/, no global install
  embeddings/models/          # Ollama model store
  embeddings/home/            # Ollama HOME and runtime state
  cache/, tmp/, logs/          # PAI-owned auxiliary files
  worker/state/               # contents of /var/lib/pai
  worker/scratch/             # contents of operator ~/.pai
  backups/<timestamp>/        # prior config, binaries, units, mount definitions
```

Only integration hooks should remain on the system drive: systemd unit files,
fstab entries and compatibility links/mountpoints. Existing `/var/lib/pai-server`
must continue to resolve to this root. Retain `/var/lib/pai` and `~/.pai` as
compatibility bind mounts so the installed worker's compiled-in paths still
work. CLI binary links can point from `/usr/local/bin` into `bin/`.

This is storage isolation, not a security sandbox: the admin worker continues
to have its existing machine access. OS dependencies and systemd's journal stay
OS-managed. Do not move the user's whole HOME or unrelated software.

## Staged implementation

1. Record service versions, paths, mount definitions and checksums. Check for
   active sessions; schedule the brief service interruption accordingly. Back
   up units and fstab before editing. Stop server and worker before copying their
   live state. Preserve ownership and permissions. Back up the stopped SQLite
   database with its WAL consistently; copying only `memory.db` while active is
   not a valid backup strategy.
2. Copy the existing binaries into `bin/`. Move worker state and scratch to the
   two new NVMe subdirectories; create persistent compatibility bind mounts with
   explicit NVMe mount dependencies. Keep a verified rollback copy on NVMe.
   Update ExecStart and WorkingDirectory to the NVMe installation. Ensure mounts
   are available before starting services; fail rather than writing into an
   unmounted eMMC directory. Restart and verify existing v0.2.5 operation first.
3. Install a pinned ARM64 Ollama distribution under `runtime/ollama`, retaining
   its matching shared libraries there. Avoid the global install script, which
   writes into system bin/lib paths. Use a separate service identity with
   traversal permission on the installation root and write permission only on
   embeddings/cache/tmp/log directories. Preserve existing secret permissions.
   Configure its service with:

   ```ini
   [Unit]
   RequiresMountsFor=/mnt/nvme/pai-server
   After=network-online.target

   [Service]
   ExecStart=/mnt/nvme/pai-server/runtime/ollama/bin/ollama serve
   WorkingDirectory=/mnt/nvme/pai-server/embeddings
   Environment="OLLAMA_HOST=127.0.0.1:11434"
   Environment="OLLAMA_MODELS=/mnt/nvme/pai-server/embeddings/models"
   Environment="HOME=/mnt/nvme/pai-server/embeddings/home"
   Environment="XDG_CACHE_HOME=/mnt/nvme/pai-server/cache/ollama"
   Environment="TMPDIR=/mnt/nvme/pai-server/tmp/ollama"
   Restart=on-failure
   ```

   Set User/Group after creating the dedicated identity; grant only the device
   group access the tested backend requires. Keep any backend library search
   override private to this service, never global. A PAI service can additionally
   use `After=ollama.service` and the same mount requirement.
4. Pull embeddinggemma into the explicit NVMe model store via the running local
   service. Test `/v1/embeddings` with multiple inputs, including a synonym-only
   retrieval example, exact identifiers and negation. Measure cold and warm
   latency, actual backend/device use, memory and EyeQ interference. L4T 39 is
   newer than the install script's explicit JetPack 5/6 branches; do not assume
   working `nvidia-smi` means this Ollama build accelerates correctly. If the
   runtime needs a local build, put source/build/cache under this same NVMe root.
5. Add only this field to the existing config, retaining providers and other
   settings:

   ```json
   "memory": {
     "mode": "semantic",
     "url": "http://127.0.0.1:11434/v1",
     "model": "embeddinggemma"
   }
   ```

   Pure semantic mode requires the feature binary. Existing v0.2.5 already
   accepts URL/model/key and can use real embeddings with its existing hybrid
   ranking; adding config alone does not install the new semantic ranking.
6. Build Linux ARM64 binaries locally (Go is not installed on the Jetson).
   Candidate builds are prepared in local `working_dir/jetson/`. Promote only
   after verifying the embedding endpoint and backing up the old state. Preserve
   truthful version/build provenance and account for worker version matching.
   Existing sessions may have replay compatibility differences from the beta
   base; test a fresh session before attempting to resume old ones.
7. Restart PAI once. Reindex all 85 items without deleting or rewriting session
   records; verify item ids/counts and retired status are retained, valid search
   excludes retired facts, /memory/stats reports the model, admin worker
   reconnects, and credentials/providers still work. Test a normal answer and
   embedding-service outage. Do not reduce recall using performance guesses.

## Rollback

Stop the affected PAI services, restore the saved binaries, config and coherent
memory database snapshot, then restart. Keep session records appended since the
snapshot; never replace or delete session history. If mount migration failed,
restore the saved unit/mount definitions and original worker data paths before
restart. The existing NVMe server bind mount need not be relocated at all.

API and runtime references: [Ollama ARM64 installation](https://docs.ollama.com/linux),
[embedding endpoint compatibility](https://docs.ollama.com/api/openai-compatibility),
and [installer JetPack detection](https://github.com/ollama/ollama/blob/main/scripts/install.sh).
