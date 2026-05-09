const siteUrl = 'https://docs.onprest.viewlegacy.com'

const routes = [
  '',
  '/quick-start',
  '/architecture',
  '/security',
  '/gateway',
  '/gateway/api-keys',
  '/gateway/configuration',
  '/gateway/logs',
  '/gateway/reverse-proxy',
  '/agent',
  '/agent/overview',
  '/agent/capability-yaml',
  '/agent/key-rotation',
  '/agent/local-logs',
  '/databases',
  '/databases/overview',
  '/databases/postgres',
  '/databases/mysql',
  '/databases/sqlserver',
  '/databases/oracle',
  '/api',
  '/api/rest',
  '/api/mcp',
  '/api/openapi',
  '/api/healthz',
  '/api/errors',
  '/operations',
  '/operations/deployment',
  '/operations/capability-changes',
  '/operations/release-gate',
  '/operations/troubleshooting',
  '/reference',
  '/reference/cli',
  '/reference/environment-variables',
  '/reference/logging',
  '/reference/test-commands'
]

export default function sitemap() {
  return routes.map((route) => ({
    url: `${siteUrl}${route}`,
    changeFrequency: 'weekly',
    priority: route === '' ? 1 : 0.7
  }))
}
