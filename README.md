# hubr

[![build status](https://badge.buildkite.com/2b4f124a3e79969bf2d7e4dd78ca21eec29ecfcec3e3b39da4.svg?branch=master&theme=00aa65,ce2554,2b74df,8241aa,fff,fff)](https://buildkite.com/myob/hubr) - `myob-oss-lab` 

## about

Command `hubr` is a git-based github release tool that provides a mechanism
for semantic versioning and changelog generation.

- List tags, releases and release assets for a GitHub repository.
- Create releases and draft releases with associated tags.
- Upload and download release assets.
- Generate new versions and changelogs.


## get hubr

Over [here](https://github.com/myob-oss/hubr/releases/latest)!

## Development

- Clone this repository
- Ensure Golang 1.x is installed (latest preferred)
- run `make install-deps` to download the current dependency versions


## Running Tests
- run `make install-deps test` to run tests

## authentication

A GitHub personal access token is required, and may be read from the environ
`GITHUB_API_TOKEN` or `TOKEN`. If neither of these return a result, `hubr` will
try to invoke a git credential helper if one is set in either local or global
git config.


## default org

If `HUBR_DEFAULT_ORG` is set in the environ, the `org` part of usages becomes
optional. It's handy if you do a lot of work with an org with a very long slug.
:wink:


## basic usage


### listing and downloading

```sh
hubr tags <repo>

hubr assets <repo>@<tag>

hubr get <repo>@<tag>:<asset> [...]
```


### tag, release and upload assets

Run in repository.

```sh
hubr release <repo>@<tag> [<upload-file>] [...]
```


### version file

The VERSION file is `hubr`'s mechanism for clutching releases on a rolling
master. It has the benefit of making the current version available in code at
build time. Quite simply, the first line of the VERSION file is the current
version. The VERSION file would typically live at the root of your repository
although alternate paths may be set using flags.

```sh
version=$(head -n 1 VERSION)
```

The workflow is like this:
- bump the version, `hubr` generates a new version string and changelog
- commit these to the VERSION file
- any non-merge commit where the current version changes is a *release commit*
- hubr will only `push` release commits, otherwise she nops.
- both bumps and pushes are idempotent and repeatable

```sh
hubr bump -w <major|minor|patch>

hubr push <repo> [<upload-file>] [...]
```

Is HEAD a release commit?
```sh
hubr now
```

What changed in the tree since the last version?
```sh
hubr what
```

*You can release on GitHub without using the VERSION file if you're not
into it.  It's also possible to lean on the VERSION file mechanism without
releasing on GitHub.*


### parallel builds

Subcommands `push` and `release` will be safe to run in parallel as long as
`push` or `release` has run once prior. GitHub releases can be created and
left in a draft state if the `-d` flag is used.


## commands

### assets

List assets for one or more release tags.
```sh
hubr assets hubr@v0.1.2
# output: hubr-darwin.zip  hubr-linux.zip  hubr-windows.zip
```

The tag defaults to the latest full release.
```sh
hubr assets hubr
# output: hubr-darwin.zip  hubr-linux.zip  hubr-windows.zip
```

Glob for assets.
```sh
hubr assets "hubr:*-linux.zip"
# output: hubr-linux.zip
```

Read from standard input.
```sh
echo hubr > manifest

hubr assets - < manifest
# output: hubr-darwin.zip  hubr-linux.zip  hubr-windows.zip
```

List with details.
```sh
hubr assets -l hubr
# output:
# hubr-darwin.zip   application/zip  4542805         
# hubr-linux.zip    application/zip  4565791         
# hubr-windows.zip  application/zip  4554776         
```


### bump

Create a new semantic version using the value of the VERSION file at head.
Run in local repo.

Emit on stdout.
```sh
hubr bump <major|minor|patch>
```

Write to version file.
```sh
hubr bump -w <major|minor|patch>
```

Use the tag of the latest github release instead of the VERSION file.
```sh
hubr bump -latest <repo> <major|minor|patch>
```


### get

Download one or more release assets to the working directory.
```sh
hubr get hubr@v0.1.2:hubr-linux.zip
```

The tag defaults to the latest full release.
```sh
hubr get hubr:hubr-linux.zip
```

Glob for assets.
```sh
hubr get "hubr:*-linux.zip"
```

Read from stdin.
```sh
echo hubr:*-linux.zip > manifest

hubr get - < manifest
```


### install

Install is suitable for artifacts which are stand-alone executables,
with support for `application/octet-stream` and `application/zip` release assets.

Binary assets (octet stream) will be downloaded to the destination and made executable.
Zip archives will be scanned for executable files which will be extracted to the destination.

Install has the same usage as get. Install's implementation is subject to further review.


### push

Release using VERSION file. Must run in repo.
Only release commits are released.

```sh
hubr push <repo> [<asset>...]
```

Draft a release.
```sh
hubr push -d <repo> [<asset>...]
```

Release a draft.
```sh
hubr push <repo>
```


### release

Release a tag! A tag will be created on GitHub if one does not exist. Runs in a
repository, unless the `-sha` flag is present. If the tag exists locally the one
created on GitHub will point to the same commit. If the tag does not exist
locally the tag created on GitHub will point to the local HEAD.

```sh
hubr release <repo>@<tag> [<asset>...]
```

Give it a nice name and stuff.
```sh
hubr release -name "new version" -body "changes were made" <repo>@<tag>
```

Get the body from a file.
```sh
hubr release -body @CHANGELOG <repo>@<tag>
```

Or Stdin.
```sh
hubr release -body - <repo>@<tag> < CHANGELOG
```

Draft a release.
```sh
hubr release -d <repo>@<tag> [<asset>...]
```

Release a draft.
```sh
hubr release <repo>@<tag>
```


### resolve

Get the tag for latest, stable or edge.
```sh
hubr resolve hubr
# output: myob-oss/hubr@v0.1.2
```

Or the web url for the release.
```sh
hubr resolve -w hubr
# output: https://github.com/myob-oss/hubr/releases/tag/v0.1.2
```

Read from stdin.
```sh
echo hubr > manifest

hubr resolve - < manifest
# output: myob-oss/hubr@v0.1.2
```

Version lock a manifest!
```sh
echo hubr > manifest

hubr resolve - < manifest > version-locked-manifest

hubr get - < version-locked-manifest
```


### tags

List full release tags for one or more repositories.  Use the `-a` flag to list
all tags including releases, pre-releases and unreleased tags.
```sh
hubr tags hubr
# output: v0.1.2      v0.1.1      v0.1.0      v0.0.2      v0.0.1
```

More details.
```sh
hubr tags -l hubr
# output:
# v0.1.2      release     2018-07-12 07:46 UTC
# v0.1.1      release     2018-07-05 12:19 UTC
# v0.1.0      release     2018-07-05 11:08 UTC
# v0.0.2      release     2018-07-05 10:22 UTC
# v0.0.1      release     2018-07-05 10:12 UTC
```

Read from stdin.
```sh
echo hubr > manifest

hubr tags - < manifest
```


### what

List changes since the last release by file path. Directories are counted.

```sh
hubr what
```

Check what changed.

```sh
hubr what <repo-file>...
# exits 0 if any file changed
```

```sh
hubr what -all <repo-file> <repo-file>...
# exits 0 if all the named files changed.
```

