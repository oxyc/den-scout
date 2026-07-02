# den-scout — multi-stage: compile TS → run the plain-JS output as a non-root Node process.
# No native deps (all I/O is global fetch), so the runtime image is just node:slim + dist + prod deps.

FROM node:22-bookworm-slim AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json tsconfig.build.json ./
COPY src ./src
RUN npm run build && npm prune --omit=dev

FROM node:22-bookworm-slim AS runtime
# Cap V8's old space below the container mem_limit (256m) so GC runs before the kernel OOM-kills us —
# Node doesn't reliably size the heap to a cgroup limit on its own.
ENV NODE_ENV=production \
    PORT=8080 \
    NODE_OPTIONS=--max-old-space-size=192
WORKDIR /app
COPY --from=build /app/node_modules ./node_modules
COPY --from=build /app/dist ./dist
COPY package.json ./

# node:*-slim ships an unprivileged `node` user (uid 1000) — run as it, never root.
USER node
EXPOSE 8080

# curl isn't in the slim image; Node's global fetch does the healthcheck. 60s interval keeps the
# every-tick cold-start of a second V8 from competing with the server under the hard mem_limit.
HEALTHCHECK --interval=60s --timeout=5s --start-period=5s --retries=3 \
  CMD node -e "fetch('http://127.0.0.1:'+(process.env.PORT||8080)+'/health').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

CMD ["node", "dist/server.js"]
