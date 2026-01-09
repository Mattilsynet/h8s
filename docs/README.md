# h8s Presentation

This folder contains a presentation about the `h8s` project, designed to be run in the terminal using [presenterm](https://github.com/mfontanini/presenterm).

## Prerequisites

You need to have `presenterm` installed.

### Installation

**macOS (Homebrew):**
```bash
brew install presenterm
```

**Linux (Arch):**
```bash
pacman -S presenterm
```

**From Source (Rust/Cargo):**
```bash
cargo install presenterm
```

### Mermaid Support (Optional but Recommended)

To view the diagrams in the presentation, you need `mermaid-cli` installed.

**Via NPM:**
```bash
npm install -g @mermaid-js/mermaid-cli
```

## Running the Presentation

To view the presentation, run the following command from this directory:

```bash
presenterm presentation.md
```

Or from the project root:

```bash
presenterm docs/presentation.md
```

## Navigation

- **Next Slide**: `l`, `j`, `Right Arrow`, `Page Down`
- **Previous Slide**: `h`, `k`, `Left Arrow`, `Page Up`
- **Quit**: `q`
