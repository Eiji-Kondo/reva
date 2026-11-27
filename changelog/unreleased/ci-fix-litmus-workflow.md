CI: Run the litmus workflow again

`test-litmus.yml` declared `needs: docker-revad-eos`, but that job lives in
`test-docker.yml`. A workflow can only depend on jobs in the same file, so
GitHub rejected the whole workflow before scheduling anything: every run
finished as a failure with zero jobs, and litmus has not executed since the
dependency was added. The litmus make targets build both images themselves
(`docker-eos-full-tests` and `docker-revad-eos` are prerequisites and compose
runs with `--build`), so the dependency was redundant as well.

https://github.com/Eiji-Kondo/reva/pull/10
