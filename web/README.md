# awp web

The dashboard `awp web` serves: every agent on the network, live. Built
with Vite, React, Tailwind, Base UI (shadcn preset b0), Beautiful UI
components, loading.dev spinners and @web-kits/audio cues.

```sh
bun install
bun run dev            # http://127.0.0.1:5173, /api proxied to 127.0.0.1:7788
AWP_WEB=http://127.0.0.1:7789 bun run dev -- --port 5174   # another host
open 'http://127.0.0.1:5173/?mock'                           # a simulated network, no backend
bun run build          # dist/, which the awp binary embeds
bun run lint && bun run typecheck
```

`src/lib/types.ts` is the API contract with `internal/web`. Components from
Beautiful UI live in `src/components/{atoms,primitives}` and
`src/app/beautifui` (copied in with `shadcn add`, then adapted); awp's own
are in `src/components/awp`.
