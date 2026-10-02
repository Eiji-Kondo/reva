Bugfix: Interpret localfs trashbin and version timestamps as milliseconds

The localfs driver names trashbin entries `<name>.d<unix-ms>` and versions
`v<unix-ms>`, but reported those values as seconds. Deletion times and version
mtimes were therefore about 1000x in the future (e.g. year 58716 in WebDAV
trashbin listings). They are now converted from milliseconds to seconds.
