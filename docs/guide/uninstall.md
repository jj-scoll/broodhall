# Uninstall

```
claude plugin uninstall guardrails@broodhall
```

This removes the plugin and, by default, its data directory, which holds the downloaded
binaries and the per-session guard state. Add `--keep-data` to keep that directory.

To remove the marketplace as well:

```
claude plugin marketplace remove broodhall
```
