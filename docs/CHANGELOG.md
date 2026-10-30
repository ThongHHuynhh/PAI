# Changelog

One section per release, newest first. An `-rc` tag ships the section of the release it is a candidate for.

## Unreleased

- Sessions are named with two words, such as `calm-otter`, instead of a hex code: easy to say, type and pass to another agent (`pai message calm-otter …`). Sessions from before keep their ids.
- Memory page reads like a notebook: one line per memory under its topic, details and actions on a click. Recalls show as plain Activity.
- The web app keeps one live connection per browser, shared by its tabs, so any number of tabs open; before, a seventh never loaded.
- The web app signs in with the whole api key that install prints.

## v0.1.0

- First release of PAI: a self-hosted server that runs agents on every machine you connect, on any model.
- Memory across sessions, models and machines. It has hybrid recall (BM25 + embeddings), a learnt recall policy (LinUCB), a usefulness filter that refuses secrets, consolidation that merges related items, and history for every change.
- Rolling compaction at 70% of the context. The full history stays in the session file.
- Accounts: several accounts per subscription, moving a session to another account or vendor, and automatic failover when an account hits its limit.
- Web app: live graph, chat, memory, settings. White and dark themes, for desktop, tablet and phone.
- Terminal app with the Orbit home screen, and a CLI.
