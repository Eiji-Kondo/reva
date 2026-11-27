Enhancement: Run the litmus workflow again

`test-litmus.yml` declared `needs: docker-revad-eos`, but that job lives in
`test-docker.yml`. A workflow can only depend on jobs in the same file, so
GitHub rejected the whole workflow before scheduling anything: every run
finished as a failure with zero jobs, and litmus has not executed since the
dependency was added. The litmus make targets build both images themselves
(`docker-eos-full-tests` and `docker-revad-eos` are prerequisites and compose
runs with `--build`), so the dependency was redundant as well.

With the workflow scheduling again, litmus failed on its first step: the EOS
test image pinned `gitlab-registry.cern.ch/dss/eos/eos-ci:02352772.el9`, a tag
CERN no longer serves (721 tags remain, 482 of them `*.el9`, none of them that
one). The pin is bumped to `58d1e5d9.el9`, which was checked to still start the
EOS daemons and answer the `eos file info /eos/user` healthcheck used by
`tests/docker/docker-compose.yml`.

https://github.com/Eiji-Kondo/reva/pull/10
