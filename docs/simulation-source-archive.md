# Private historical source archive

The $10 research cap remains removed. Archival changes storage only. It cannot
qualify data, release a transport hold, enable trading, or produce performance.

MSI's external exFAT volume presents regular files as mode 0700. Existing
acquisition checks require exact mode 0600, owner UID, one hard link, a regular
file, and descriptor-based hashes. The kernel 6.8 overlay prototype rejected
this backing filesystem. No permission check was relaxed and no volume was
formatted.

The accepted component uses a privately extracted, SHA256-pinned bindfs
1.14.7-1.1build2 executable. No system package was installed. Its owner-only,
read-only view presents files as 0600 and directories as 0700, with
`nosuid,nodev,noexec,default_permissions` and no `allow_other`. The exact lower
source path and mount identity are checked. Physical copies remain private on
the volume; their direct mode 0700 paths are unsuitable for acquisition checks.

`scripts/archive-research-source.py` copies one complete, flat research source
directory to the external archive. It checks every original and copied member,
syncs files and directories, writes a durable intent before renaming, retains
the complete original in a private backup, and mounts the view at the original
source path. Normal failure restores that original directory. Partial or
different old copies fail closed and require explicit reconciliation.

`scripts/restore-research-source.py` verifies the durable intent, pinned
executable and physical copy before recreating an unmounted view. Its default
recovery requires the preserved original. Explicit `--archive-only` recovery
allows a missing or partial backup, leaves it untouched, and mounts only at an
absent or empty private target. Backup presence is reported separately from
verification. Repeated verification of an already mounted correct view is
idempotent. A stale or disconnected mount requires an explicit normal unmount;
the helper never forces one. There is no automatic reboot or service recovery.

`scripts/release-research-backup.py` verifies the historical archive intent and
report, every physical copy, the read-only view, and every remaining backup
member before recording a durable release intent. It deletes only enumerated
duplicate files from the exact private backup directory. Unknown or changed
members block release. After an interrupted release, use a fresh output path;
the remaining subset is verified again. Archive-only recovery is independent
of that subset. The historical archive report remains an immutable record of
the original operation.

Cycle 30 validated ten synthetic permission, descriptor, durability, and
read-only controls. The actual first-day stock source contains seven files
and 167,439,269 bytes. Its repeated verification and explicit unmount/remount
recovery passed. After recovery, all eight acquired-source proofs passed and
the complete Go comparison matched 9,786,851 stock records across 23 fields
against the pinned independent reference. Original receipt paths and source
bytes remained unchanged.

Cycle 32 passed eleven synthetic controls, including unknown/changed member
rejection, a wrong binder, an occupied recovery target, an active reader,
missing-backup recovery, and interrupted-release reconciliation. Actual stock
recovery passed with the complete original temporarily parked. The verified
seven-file duplicate was then released, reclaiming 167,439,269 bytes of source
content. The licensed external copy and original source paths remain intact.
Complete compressed capacity for all 126 sessions is unverified. The helper's
observable open-descriptor scan is not a global reader lock; relocation must
be scheduled at an explicitly quiescent research boundary. Current checks do
not establish crash tolerance of the removable drive or full replay readiness.
The scan permits only the exact lower-directory descriptor held by the
SHA256-pinned bindfs executable; archive-member readers still block release.

Evidence is in `research/simulation/2026-10-05/cycle30-*` and `cycle32-*`. Provider bodies,
signed links, credentials, and licensed record vectors stay private on MSI.

References: [pinned bindfs manual](https://github.com/mpartel/bindfs/blob/1.14.7/src/bindfs.1),
[Linux 6.8 overlay documentation](https://github.com/torvalds/linux/blob/v6.8/Documentation/filesystems/overlayfs.rst).
