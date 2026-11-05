# Roll out to a team

Give a team the broodhall marketplace and plugins without each person typing the install
commands. There are two routes. Use a project file for one repository, and managed settings
for the whole organisation.

`guardrails team-init` prints the block for you. It prints only JSON on stdout, so you can
redirect it. It says where to paste on stderr. Install guardrails and start one session
first, so the binary is downloaded. It is not on your `PATH`; see
[Run it from a terminal](../guardrails/index.md#run-it-from-a-terminal).

## One repository

```sh
guardrails team-init > broodhall-settings.json
```

Merge the result into `.claude/settings.json` in the repository and commit it:

```json
{
  "enabledPlugins": {
    "context-guard@broodhall": true,
    "guardrails@broodhall": true,
    "memory@broodhall": true
  },
  "extraKnownMarketplaces": {
    "broodhall": {
      "autoUpdate": true,
      "source": {
        "repo": "jj-scoll/broodhall",
        "source": "github"
      }
    }
  }
}
```

Each teammate accepts the folder trust prompt once. Claude Code then registers the marketplace
and installs the plugins that live in this repository: guardrails, memory and context-guard.
Until they accept it, the marketplace entry is ignored.

!!! warning "rtk is not installed this way"
    rtk comes from another repository. Claude Code does not install a plugin from an external
    source on the strength of repository settings alone. If you add `-plugins guardrails,rtk`,
    each person must also run `claude plugin install rtk@broodhall` once. Until they do, Claude
    Code reports that rtk is enabled in project settings but not installed.

- `extraKnownMarketplaces` maps a marketplace name to a `source` and an optional
  `autoUpdate`. The `github` source takes `repo`. Other sources are `git` (a `url`), `url`,
  `file`, `directory` and `settings`.
- `enabledPlugins` maps `plugin@marketplace` to true or false.
- `autoUpdate: true` makes Claude Code refresh the marketplace and update the installed
  plugins together after startup. Without it, a third-party marketplace never updates by
  itself, and plugins drift apart (see [Version skew](#version-skew)). `team-init` sets it
  to true by default; `-auto-update=false` sets it to false.
- Pick plugins with `-plugins`, for example `-plugins guardrails,memory`. The names are
  `guardrails`, `memory`, `context-guard` and `rtk`.

The same block works in your own `~/.claude/settings.json`.

## The whole organisation

```sh
guardrails team-init -managed -domains acme.com > managed-settings.json
```

Save it as `managed-settings.json` in the system directory for the operating system:

| OS | Path |
| --- | --- |
| macOS | `/Library/Application Support/ClaudeCode/managed-settings.json` |
| Linux and WSL | `/etc/claude-code/managed-settings.json` |
| Windows | `C:\Program Files\ClaudeCode\managed-settings.json` |

Or drop it into a `managed-settings.d/` directory beside that file, as a numbered file such as
`20-broodhall.json`. Claude Code merges `managed-settings.json` first, then the drop-ins in
alphabetical order. MDM and server-managed settings are other ways to deliver the same keys.

```json
{
  "enabledPlugins": { "...": "as above" },
  "extraKnownMarketplaces": { "...": "as above" },
  "pluginConfigs": {
    "guardrails@broodhall": {
      "options": { "allowed_email_domains": "@acme.com" }
    }
  }
}
```

- Managed settings sit above every other level. A plugin set to true there is force-enabled,
  and users cannot turn it off.
- `-domains` adds `pluginConfigs`, which sets the guardrails `allowed_email_domains` option
  for everyone. Claude Code reads `pluginConfigs` only from user and managed settings, so
  `-domains` needs `-managed`.
- To also stop people adding other marketplaces, add `strictKnownMarketplaces`. It is managed
  only. `team-init` does not print it, because it is a policy choice. See the Claude Code
  [settings reference](https://code.claude.com/docs/en/settings-reference#strictknownmarketplaces).
- `guardrails doctor` reads the managed settings files at the paths above, so a plugin enabled
  only there does not draw a "plugin enabled" warning. Server-managed settings and MDM
  profiles are not files doctor can read, so a plugin enabled only through those still gets
  that `WARN` at every session start.
- Config-gate does not watch managed settings. It judges the user, project and local settings
  files, so a change to `observe` or `allowed_email_domains` in `managed-settings.json` is not
  checked. Only an administrator can write that file.

## After the rollout

Each person runs `/guardrails:setup` once to pick a profile. See [Setup](../guardrails/setup.md).

## Version skew

All three broodhall plugins ship from one release. If one updates and another does not,
for example memory 1.6.0 with guardrails 1.5.3, a hook can expect something the other plugin
lacks. `guardrails doctor` checks for this with two warnings, shown at session start:

- The installed broodhall plugins in `plugins/installed_plugins.json` are at more than one
  version. The message names the plugins behind and the `claude plugin update` command for each.
  Only user-scope installs count, plus project or local installs recorded for the project you
  are in. An install made for another repository is not loaded here and is never reported.
- An installed plugin differs from the version the marketplace checkout on disk offers. Doctor
  reads the checkout under `plugins/marketplaces/`, or the `installLocation` that
  `known_marketplaces.json` records. It does not read a `ref` or `sha` from your settings.

Fix either with `claude plugin update NAME@broodhall`, then `/reload-plugins`. See
[Doctor](../guardrails/doctor.md) for the details.

## Sources

The key names and shapes come from the Claude Code docs:

- [Settings reference: extraKnownMarketplaces](https://code.claude.com/docs/en/settings-reference#extraknownmarketplaces)
- [Settings reference: enabledPlugins](https://code.claude.com/docs/en/settings-reference#enabledplugins)
- [Settings reference: pluginConfigs](https://code.claude.com/docs/en/settings-reference#pluginconfigs)
- [Deploy managed settings](https://code.claude.com/docs/en/managed-settings)
- [Plugin loading reference](https://code.claude.com/docs/en/plugins/loading)
