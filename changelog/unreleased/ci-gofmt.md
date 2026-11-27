Enhancement: Format the tree with gofmt and check it on pull requests

Fifteen files had drifted from `gofmt`: trailing whitespace in `ocdav.go`,
`meta.go`, the LDAP user and group handlers and `report.go`, plus import order
in `ocmshareprovider.go`. Nothing caught it, because the workflow sat one
directory too deep (`.github/workflows/.github/workflows/gofmt.yml`) and was
never registered with GitHub. The tree is formatted, and a new workflow runs
`gofmt -l` on every pull request and push.

https://github.com/Eiji-Kondo/reva/pull/3
