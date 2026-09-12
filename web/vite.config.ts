import { existsSync, readFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { resolve } from 'node:path'
import react from '@vitejs/plugin-react'
import { loadEnv } from 'vite'
import { defineConfig } from 'vitest/config'

// Shared mkcert certificate (*.dev.ecoworks.ca, localhost); plain HTTP when absent.
const certDir = resolve(homedir(), 'Code/.traefik/certs')
const certPath = resolve(certDir, 'cert.pem')
const keyPath = resolve(certDir, 'key.pem')
const https =
  existsSync(certPath) && existsSync(keyPath)
    ? { cert: readFileSync(certPath), key: readFileSync(keyPath) }
    : undefined

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  // The api-gateway REST port (compose publishes it with the same certificate).
  const apiTarget = env.API_PROXY_TARGET || 'https://localhost:3134'
  const proxy = { '/v1': { target: apiTarget, changeOrigin: true, secure: false } }
  return {
    plugins: [react()],
    server: {
      port: 3034, // registered in ~/.claude/PORTS.md (sumpnet web dashboard)
      strictPort: true,
      host: true,
      https,
      allowedHosts: ['dev.ecoworks.ca', 'localhost'],
      proxy,
    },
    preview: { port: 3034, strictPort: true, host: true, https, allowedHosts: ['dev.ecoworks.ca', 'localhost'], proxy },
    test: { environment: 'node', include: ['src/**/*.test.ts'] },
  }
})
