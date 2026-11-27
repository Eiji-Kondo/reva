Bugfix: Move the drive eTag when a write lands below the drive root

The localdb cache copied a directory's mtime when it wrote the row and never
re-read it, so a write below a directory left every row above it stale. A client
polling `/graph/v1beta1/me/drives` reads the eTag of the drive root row, so the
change was invisible to it even though localfs had already Chtimes'd its way to
the root on CreateDir, Delete, Move and RestoreRecycleItem.

Writes now re-read every ancestor row up to and including the root, and the row
upsert follows the path rather than the inode so an upload can replace a file.
Deletes purge rows without writing any, so they refresh the ancestors
themselves. Uploads also resolve the tus session destination before delegating:
FinishUpload removes the session file, so the previous resync stat'ed a path that
no longer existed and never refreshed the row.

localfs.FinishUpload additionally calls propagate after the rename. Without it
an upload moved only its direct parent on disk, so a file that landed deeper
than the home never moved the home mtime at all.

https://github.com/Eiji-Kondo/reva/pull/2
