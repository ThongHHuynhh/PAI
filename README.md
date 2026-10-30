<picture>
  <source media="(max-width: 700px), (hover: none) and (pointer: coarse)" srcset="https://raw.githubusercontent.com/pai-ai/github-media/main/hero-mobile.svg">
  <img src="https://raw.githubusercontent.com/pai-ai/github-media/main/hero-desktop.svg" alt="PAI, with the biplane flying over the clouds" width="960">
</picture>
<div><h3>PAI is an open-source agent harness</h3></div>
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/pai-ai/github-media/main/tui-dark.svg">
  <img src="https://raw.githubusercontent.com/pai-ai/github-media/main/tui-light.svg" alt="The PAI TUI: a sidebar of sessions on different models, and an agent that moves a Qwen fine-tune from a GPU server to a Mac Studio and quantizes it to run there" width="960">
</picture>

# Self-Hosting Quickstart

Run this command on your server machine (any always-on machine will do):

```shell
# Linux, macOS
curl -fsSL https://pai.ai/install | sh -s -- server quickstart

# Windows
& ([scriptblock]::Create((irm https://pai.ai/install.ps1))) server quickstart
```

The quickstart walks you through adding a provider, workers (computers your agents control) and connectors (ways to message your agents).

Read more in [docs/self-hosting.md](docs/self-hosting.md).

<br><br>

# Features
### Model agnostic
- PAI can use any model from any provider, including Anthropic, OpenAI, Google, and more.

### Context efficient
- PAI agents have a restricted set of tools and manage their own context by retrieving key information from other sessions.

### Connects all your devices
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/pai-ai/github-media/main/architecture-3d-dark.svg">
  <img src="https://raw.githubusercontent.com/pai-ai/github-media/main/architecture-3d-light.svg" alt="The PAI Architecture: connectors reach PAI Server, which holds secrets and controls, observability and the file server; the network joins the server and every worker" width="960">
</picture>

- PAI links agents and computers with fine-grained security controls. No Tailscale needed.

### Shared file drives
- PAI sets up a remote filesystem you can use concurrently without risk of de-sync.

### Browser and computer use
- PAI drives your real browsers and applications with native computer use.

# FAQ
<details>
<summary>Can I bring my Claude Code or ChatGPT subscription?</summary>

Yes, PAI can use the regular limits of a Claude Code or ChatGPT Codex subscription, or an API key with any model.
</details>

<details>
<summary>Does PAI share my data?</summary>

No data is shared by default. Users can opt in to sharing data to improve PAI in /settings.
</details>

<details>
<summary>Do I need more than one device?</summary>

No. Everything can run on one device.
</details>

<details>
<summary>Does pai-server need Linux?</summary>

No. pai-server runs on macOS, Windows and Linux. Hosting shared files requires a compatible connected Linux device.
</details>

<details>
<summary>Do you take contributions?</summary>

We welcome feedback as GitHub issues. Pull requests are open to collaborators only. Any agent can POST plain text to https://bugs.pai.ai to report a bug.
</details>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/pai-ai/github-media/main/footer-still-dark.svg">
  <img src="https://raw.githubusercontent.com/pai-ai/github-media/main/footer-still-light.svg" alt="A billboard turns through what PAI agents do, while the biplane tows a banner: Now you're flying!" width="960">
</picture>
