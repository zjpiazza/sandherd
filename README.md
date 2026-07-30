<a id="readme-top"></a>

[![Project Status][status-shield]][roadmap-url]
[![Contributors][contributors-shield]][contributors-url]
[![Forks][forks-shield]][forks-url]
[![Stargazers][stars-shield]][stars-url]
[![Issues][issues-shield]][issues-url]

<br />
<div align="center">
  <img src="docs/images/sandherd.png" alt="Sandherd: Kubernetes agents running in isolated sandboxes" width="640" />

  <h1>Sandherd</h1>

  <p align="center">
    <strong>Isolated agents. Unified control.</strong>
    <br />
    A Kubernetes-native control plane for running persistent AI agents in individual sandboxes.
    <br />
    <br />
    <a href="https://github.com/zjpiazza/sandherd/issues/new?labels=bug">Report a bug</a>
    &middot;
    <a href="https://github.com/zjpiazza/sandherd/issues/new?labels=enhancement">Request a feature</a>
  </p>
</div>

<details>
  <summary>Table of Contents</summary>
  <ol>
    <li>
      <a href="#about-the-project">About the Project</a>
      <ul>
        <li><a href="#architecture">Architecture</a></li>
        <li><a href="#design-principles">Design Principles</a></li>
        <li><a href="#contracts">Contracts</a></li>
        <li><a href="#built-with">Built With</a></li>
      </ul>
    </li>
    <li>
      <a href="#getting-started">Getting Started</a>
      <ul>
        <li><a href="#prerequisites">Prerequisites</a></li>
        <li><a href="#installation">Installation</a></li>
      </ul>
    </li>
    <li><a href="#usage">Usage</a></li>
    <li><a href="#development">Development</a></li>
    <li><a href="#roadmap">Roadmap</a></li>
    <li><a href="#contributing">Contributing</a></li>
    <li><a href="#license">License</a></li>
    <li><a href="#acknowledgments">Acknowledgments</a></li>
  </ol>
</details>

## About the Project

Sandherd provides one control plane for creating, observing, and interacting with AI agents that run in separate Kubernetes sandboxes.

Coding agents need more than short-lived command execution. They need durable filesystems, long-running processes, reconnectable terminals, scoped credentials, resource limits, and strong isolation from both the host and other agents. Sandherd combines those capabilities behind a client-neutral API.

[Herdr](https://herdr.dev/) is the first interactive integration, but it is not the platform boundary. CLIs, web applications, IDEs, and automation can use the same lifecycle and terminal APIs.

Coding-agent implementations are integrations too. Sandherd selects a versioned runtime adapter for tools such as Codex, Claude Code, OpenCode, Pi, or Grok Build while keeping the lifecycle API independent of any one tool. The agent harness, its LLM provider, and its credential profile are separate concepts.

### Architecture

```mermaid
flowchart LR
    H[Herdr integration]
    C[CLI / automation]
    W[Web and IDE clients]

    H --> API
    C --> API
    W --> API

    API[Sandherd control plane<br/>REST + events + terminal streams]
    API --> AS[Kubernetes Agent Sandbox]

    AS --> A[Agent sandbox A]
    AS --> B[Agent sandbox B]
    AS --> N[Agent sandbox N]
```

The control plane manages durable agent lifecycle through a REST API. Interactive terminal attachments use reconnectable WebSocket streams. Kubernetes and Agent Sandbox remain implementation details rather than requirements imposed on every client integration.

### Design Principles

- **One agent, one sandbox.** Each agent receives an independent runtime, resource budget, and filesystem.
- **Durable agents, ephemeral attachments.** Closing a terminal must not terminate the agent behind it.
- **Client-neutral control.** Herdr is an integration, not the infrastructure API.
- **Agent-neutral runtime.** Tool-specific launch, authentication, session, and health behavior belongs in versioned adapters.
- **Private by default.** Sandboxes are internal workloads with explicit ingress and controlled egress.
- **Kubernetes-native lifecycle.** Scheduling, storage, policies, and runtime isolation use existing Kubernetes primitives.
- **Progressive isolation.** Start with containers and opt into gVisor or Kata Containers where stronger boundaries are required.

### Contracts

- [REST lifecycle API](api/openapi.yaml)
- [Terminal attachment protocol](api/terminal.md)
- [In-sandbox runner protocol and operations](docs/runner.md)
- [Durable workspaces and repository bootstrap](docs/workspaces.md)
- [Lifecycle control plane](docs/control-plane.md)
- [Security model and operations](docs/security.md)
- [Herdr integration](docs/herdr-integration.md)
- [Resource and API model](docs/adr/0001-resource-and-api-model.md)
- [Agent Sandbox cluster foundation](docs/platform/agent-sandbox.md)

### Built With

- [Go](https://go.dev/)
- [Kubernetes](https://kubernetes.io/)
- [Kubernetes Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
- [Herdr](https://herdr.dev/)
- [Tailscale](https://tailscale.com/) for optional private access

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Getting Started

> [!IMPORTANT]
> Sandherd is currently pre-alpha. The architecture is being validated and there is not yet a supported release.

### Prerequisites

The initial deployment target requires:

- A Kubernetes cluster
- `kubectl` access
- A default `StorageClass`
- The Agent Sandbox controller and extensions
- A CNI that enforces Kubernetes `NetworkPolicy`

gVisor or Kata Containers are recommended for running untrusted workloads, but they will not be required for the first development deployment.

Developing Sandherd itself requires Go 1.26 or newer, Node.js 24 for contract validation, and Docker with BuildKit for container builds. See the [local development guide](docs/development.md) for the repository layout and commands.

### Installation

Installation instructions will be added after the first end-to-end release. The intended installation path is a Helm chart with optional GitOps examples.

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Usage

The planned workflow is deliberately independent of any particular user interface:

1. A client requests a new agent from the Sandherd API.
2. Sandherd creates or claims an isolated Kubernetes sandbox.
3. A runner starts the selected agent adapter inside a persistent PTY.
4. Herdr or another client attaches to the terminal stream.
5. The client can disconnect and reconnect without stopping the agent.
6. Sandherd suspends, resumes, or deletes the sandbox according to policy.

The Herdr integration maps panes and agent states onto this lifecycle while keeping Kubernetes credentials out of the Herdr client. Selecting Codex, Claude Code, OpenCode, Pi, or Grok Build changes the runtime adapter, not the client-facing lifecycle.

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Development

Run the complete local verification suite with one command:

```sh
make verify
```

The lifecycle `control-plane`, reconnectable `runner`, and `herdr-bridge` command build locally and expose `--version` without contacting Kubernetes or any other external dependency. Detailed setup, build, container, and CI instructions are in the [development guide](docs/development.md).

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Roadmap

### Control-plane foundation

- [x] Install Agent Sandbox and validate persistent claims
- [x] Define the Sandherd agent and attachment APIs
- [x] Build the reconnectable sandbox runner
- [x] Create, inspect, stop, resume, and delete agents through the control plane
- [x] Stream an interactive terminal over WebSocket
- [x] Ship the first Herdr integration
- [x] Add PVC-backed agent workspaces
- [x] Add scoped secrets and default-deny network policies

### Agent runtimes

- [ ] Define the provider-neutral adapter and credential framework ([#25](https://github.com/zjpiazza/sandherd/issues/25))
- [ ] Run Codex with durable ChatGPT subscription authentication ([#26](https://github.com/zjpiazza/sandherd/issues/26))
- [ ] Support Claude Code, including subscription credentials ([#27](https://github.com/zjpiazza/sandherd/issues/27))
- [ ] Support OpenCode with selectable model providers ([#28](https://github.com/zjpiazza/sandherd/issues/28))
- [ ] Support the Pi coding agent with selectable model providers ([#29](https://github.com/zjpiazza/sandherd/issues/29))
- [ ] Support the xAI Grok Build coding agent ([#30](https://github.com/zjpiazza/sandherd/issues/30))

### Operations and release

- [ ] Add gVisor runtime support ([#11](https://github.com/zjpiazza/sandherd/issues/11))
- [ ] Add warm pools, suspension, observability, and load tests ([#12](https://github.com/zjpiazza/sandherd/issues/12))
- [ ] Publish and verify the first packaged release ([#10](https://github.com/zjpiazza/sandherd/issues/10))

See the [open issues][issues-url] for proposed features and known issues.

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Contributing

Sandherd is at an early stage, so design feedback and focused prototypes are especially valuable. Please read [CONTRIBUTING.md](CONTRIBUTING.md) for branch names, required checks, commit guidance, and issue-linked pull requests. Open an issue before beginning a large architectural change so the approach can be discussed first.

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## License

The project license has not been selected yet. A license will be added before the first public release.

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Acknowledgments

- [Kubernetes SIG Apps Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
- [Herdr](https://herdr.dev/)
- [Tailscale Kubernetes Operator](https://tailscale.com/docs/kubernetes-operator)
- [Best README Template](https://github.com/othneildrew/Best-README-Template)

<p align="right">(<a href="#readme-top">back to top</a>)</p>

[status-shield]: https://img.shields.io/badge/status-pre--alpha-orange?style=for-the-badge
[roadmap-url]: https://github.com/zjpiazza/sandherd#roadmap
[contributors-shield]: https://img.shields.io/github/contributors/zjpiazza/sandherd.svg?style=for-the-badge
[contributors-url]: https://github.com/zjpiazza/sandherd/graphs/contributors
[forks-shield]: https://img.shields.io/github/forks/zjpiazza/sandherd.svg?style=for-the-badge
[forks-url]: https://github.com/zjpiazza/sandherd/network/members
[stars-shield]: https://img.shields.io/github/stars/zjpiazza/sandherd.svg?style=for-the-badge
[stars-url]: https://github.com/zjpiazza/sandherd/stargazers
[issues-shield]: https://img.shields.io/github/issues/zjpiazza/sandherd.svg?style=for-the-badge
[issues-url]: https://github.com/zjpiazza/sandherd/issues
