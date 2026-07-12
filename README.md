# Golang library for CI/CD workflows

[![lib-test](https://github.com/megalomania428/go-lib-ci/actions/workflows/lib-test.yaml/badge.svg)](https://github.com/megalomania428/go-lib-ci/actions/workflows/lib-test.yaml)

## Make release

- clone me:

```bash
git clone --recursive git@github.com:megalomania428/go-lib-ci.git go-lib-ci
```

- make tag and send to release:

```bash
git checkout master && git pull
git tag -fm $(git branch --sho) 1.0.0 && git push --force origin $(git describe)
```
