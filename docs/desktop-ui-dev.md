# Desktop UI Development Guide

This guide is for contributors who want to develop the desktop wallet UI.

## Architecture at a glance

The wallet binary (`caribou`) interacts with the browser via an HTTP server (`wallet/appserver`)
that serves a single-page React app. The web assets are built using a node script
that runs Webpack and TypeScript, compiling the .tsx and .scss files into the `desktop/dist/` folder.
The build output in `desktop/dist/` is embedded (`go:embed`) into the `caribou` binary by
`wallet/appserver/embed_desktop.go`, and the fonts and images in `shared/` by
`wallet/appserver/embed.go`.

The React app communicates with the wallet core through the HTTP server via two mechanisms:

- **REST**: `POST /api/...` for commands (JSON in, JSON out).
- **WebSocket**: `/ws` for `CoreNote` push notifications (balance, sync, tx,
  rate, tip-change, etc.).

## Repository layout

```text
wallet/appserver/
├── appserver.go        # chi router, TLS, CSRF, cookies, session keys
├── api.go              # REST handlers — /api/* surface
├── embed.go            # //go:embed shared/img shared/font
├── embed_desktop.go    # //go:embed desktop/dist (the mobile tag uses embed_mobile.go)
├── jsintl.go           # registers the UI locale strings with the intl package
├── locales/            # old site UI translations, a source for importlocales only
├── package.json        # npm workspace root: shared, desktop, mobile
├── shared/             # code and assets common to the desktop and mobile UIs
│   ├── src/            # application.ts, registry.ts, http.ts, ws.ts, intl.ts, …
│   ├── img/            # images served at /img/ (embedded)
│   └── font/           # web fonts served at /font/ (embedded)
├── mobile/             # the mobile React app
└── desktop/            # the desktop React app
    ├── package.json
    ├── webpack.config.js
    ├── tsconfig.json
    ├── eslint.config.js
    ├── locales/        # Go locale maps, served to the SPA via POST /api/locale
    └── src/
        ├── index.tsx           # ReactDOM root, mounts <App/>
        ├── App.tsx             # top-level state machine
        ├── components/         # one .tsx per screen / widget
        └── scss/               # Sass entry: application.scss + partials
```

## Build pipeline

The SPA is built with webpack and TypeScript (`ts-loader`). Sass is compiled
via `sass-loader` using the modern API. Lint runs via `eslint-webpack-plugin`
during the build, so a lint error fails the build.

```bash
cd wallet/appserver
npm ci                           # one-time / after lockfile changes
npm run build --workspace=desktop  # writes desktop/dist/bundle.js + dist/index.html
```

You need Go 1.25 or later and Node.js 20.19+ or 22.13+ (the versions CI tests).

The UI is an npm workspace with a single lockfile at `wallet/appserver`, so
install from there; `npm ci` run inside `desktop/` alone skips the shared
toolchain and the build fails with `webpack: command not found`.

A fresh checkout holds only a placeholder in `desktop/dist`, so Go builds and
tests of `wallet/appserver/...` compile without the bundle, but the resulting
binary serves no UI until you run `npm run build`. CI builds the bundle before
tests run; do the same locally to see the UI.

**If you are doing significant work on the UI**, you will want to run
`npm run watch` instead of `npm run build`, and pass the `--no-embed-site` flag to `caribou`.
The `watch` script will monitor the `src` directory and rebuild the web assets when changes are made.

## Getting Started

For significant UI work, it is highly recommended to run on **simnet**.
See the [simnet development guide](simnet-dev.md) for more information on how to test on simnet.
Run at least one or two simnet harnesses before starting your UI dev work.

You'll run two separate terminals (in addition to any simnet harnesses).

### Terminal 1 - Web Assets

```bash
cd wallet/appserver/desktop
npm run watch
```

### Terminal 2 - Caribou

```bash
cd wallet/cmd/caribou
go build && ./caribou --simnet --no-embed-site
```

To start with a fresh wallet, shut down `caribou`, delete the files in `~/.caribou/simnet`, and restart.

## App lifecycle

`src/index.tsx` mounts `<App/>` into `#root`. `App.tsx` is a small state
machine on `AppState`:

```text
fetchAppState() ──► AppState
                     ├── user != null         → <MainFrame/>
                     ├── inited && !user      → <UnlockForm/>
                     └── !inited              → <InitWizard/>
```

`Loading` renders while the initial fetch is in flight.

`MainFrame` owns the persistent shell (sidebar + top bar + active page) and
delegates to one of the page components (`Portfolio`, `Trade`, `Settings`,
`Activity`). `Send`, `Receive`, and `Swap` are modals, rendered through
`Modals.tsx` and triggered with `app.setForm(...)`.

## The `app` singleton (`js/application.ts`)

There is exactly one `Application` instance, imported as `app` everywhere. It
owns:

- **User & app state** — `app.user`, `app.inited`, `app.authed`,
  `app.commitHash`, `app.langs`, `app.lang`.
- **WebSocket** — set up in `start()`; routes `CoreNote`s into per-route
  receivers stored on `app.noteReceivers`.
- **Balance / fiat updaters** — components register themselves with
  `app.balanceUpdaters[assetID] = fn` and get re-rendered on `balance` /
  `fiatrateupdate` notes.
- **Loggers** — runtime-toggleable, surface on `window`:

  ```js
  window.enableLogger('mylogger', true)
  window.recordLogger('mylogger', true)   // start buffering
  window.dumpLogger('mylogger')           // returns the buffer
  ```

  Settings persist via `State.storeLocal`. Use `app.log('mylogger', ...)` from
  TypeScript.

- **Setform** — `app.setForm({...})` opens the global modal. Components
  register a receiver via `app.setForm = ...` (see `MainFrame.tsx`).

Components that need to react to lifecycle events do so via `useEffect`,
subscribing/unsubscribing on the `app.*Updaters` and `app.noteReceivers` maps.

## Talking to the backend

Use `postJSON` / `getJSON` from `js/http.ts`. They always resolve — never
throw — and return an `Envelope`:

```ts
type Envelope = { ok: boolean; errMsg?: string; ...payload }

const res = await postJSON('/api/txfee', { addr, value, assetID, subtract })
if (!res.ok) {
  // res.errMsg is always a string here
  return showError(res.errMsg)
}
// res has the payload fields documented by the handler
```

## Internationalization

Strings live in **two** places:

1. **Go-side locale maps** under `wallet/appserver/locales/<lang>.go`.
   Aggregated by `wallet/appserver/desktop/locales/locales.go` and served via
   `POST /api/locale`. The SPA fetches its language at startup
   (`intl.loadLocale`) and caches it in `localStorage` keyed on
   `(lang, commitHash)`.
2. **Inline `t(slug)` calls** in components and `js/*.ts`. The slug is the
   map key; the Go map value (`{T: "..."}`) is the English source.

To add a new string:

1. Add the entry to `locales/en-us.go` (and ideally to the other locales).
2. Use `t('your-slug')` from `js/intl.ts` in the component.
3. Rebuild the SPA; the new key flows out through `/api/locale`.

`tvar('slug', {name: 'Brian'})` does `{name}` substitution; `tdom` returns a
DOM fragment for HTML-containing translations.

Missing translations are logged to console; enable
`window.recordMissingTranslations(true)` and call
`window.dumpMissingTranslations()` to extract a list for a translator
worksheet.

`caribou` must be restarted after adding a new entry under `appserver/locales` before
the entry is returned via `/api/locale`.

## Styling

Sass with custom partials. The entry point is `scss/application.scss`, which
imports the rest:

- `vars.scss` — colors, spacing, breakpoints. **Add new design tokens here**
  rather than hard-coding in components.
- `mixins.scss` — reusable Sass mixins (pulled in via `base.scss`, not
  `application.scss` directly).
- `base.scss` — element-level resets and defaults.
- `typography.scss` — font stacks, sizes, headings.
- `components.scss` — component classes (the bulk of styles).
- `utilities.scss` — atomic helpers (`d-flex`, `mt-2`, etc., bootstrap-like).
- `icons.scss`, `animations.scss` — what they say.

There is no CSS-in-JS. Style components by giving them semantic class names
and adding rules in `components.scss`. The convention is BEM-lite: a
component owns a block class (e.g. `.portfolio`) and nests its element/state
classes under it.

## Adding a new screen or component

1. Create `src/components/Foo.tsx`. Default-export a function component.
2. If it's a page, register a route in `MainFrame.tsx` (or wherever the active
   page is resolved from `app.pageData.pageRoot`).
3. If it needs new types, add them to `js/registry.ts` — keep types in one
   place so the rest of the codebase can import without circulars.
4. If it needs a new API endpoint, add the handler in `wallet/appserver/api.go`
   and register the route in the `mux.Route("/api", ...)` block in
   `appserver.go`.
5. Add CSS in `scss/components.scss`.
6. Add user-facing strings to `wallet/appserver/locales/en-us.go` and call
   them via `t(...)` or `tvar(...)`.
7. Remember to rebuild web assets (`npm run watch` or `npm run build`).

## Lint and pre-push hygiene

```bash
cd wallet/appserver/desktop
npm run lint    # tsc + eslint
```

The lint config extends `standard`, `@typescript-eslint/recommended`, and
`plugin:react/recommended`, with a small set of overrides in `.eslintrc.js`.
`no-explicit-any` is off (intentionally — there's a lot of `any` in the core
note plumbing), but write narrow types where you reasonably can.

Webpack's `ESLintPlugin` runs lint at build time and emits errors as build
errors, so `npm run build` is the strict gate.

On the Go side, follow the repo-wide rule: `gofmt` and `golangci-lint -c
./.golangci.yml run` must pass before pushing anything that touches `.go`
files, including the appserver and locale files.
