# @latexmk/dashboard

A Preact/Vite administration console for the job queue, server capabilities, and administrator user/token management.

```sh
pnpm --filter @latexmk/dashboard dev
```

The development server proxies `/v1` to `http://127.0.0.1:8080` by default. Set `LATEXMK_API_ORIGIN` to change the proxy target, or set `VITE_API_BASE_URL` when the deployed API uses a different origin. The token is stored only in the operator's browser local storage.

When Dashboard and API have different origins, configure the API with the Dashboard's exact origin in `LATEXMK_CORS_ORIGINS`. A same-origin reverse proxy does not need this setting.
