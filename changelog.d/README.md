# Changelog entries

A pull request that changes shipped files adds one new file here, named after its branch
(`fix-memory-socket.md`), holding one bullet per user-facing change:

```markdown
- memory: what a user notices, in their terms. (#NN)
```

The release moves every file here except this one into
[`docs/guide/changelog.md`](../docs/guide/changelog.md) under the version it cuts, in file-name
order, and deletes them. Never edit the changelog itself in a pull request. A file per pull
request means two open pull requests, or a pull request and a release, never edit the same lines,
so an entry cannot be lost resolving a merge conflict. See
[Pull requests](../docs/reference/release.md#pull-requests).
