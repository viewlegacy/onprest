const siteUrl = 'https://docs.onprest.viewlegacy.com'

const routes = [
  '',
  '/quick-start',
  '/architecture',
  '/security',
  '/capabilities',
  '/capabilities/inputs-and-outputs',
  '/capabilities/policy-and-limits',
  '/capabilities/databases',
  '/capabilities/databases/postgres',
  '/capabilities/databases/mysql',
  '/capabilities/databases/sqlserver',
  '/capabilities/databases/oracle',
  '/capabilities/csv',
  '/capabilities/files',
  '/capabilities/http',
  '/capabilities/code',
  '/capabilities/code/go',
  '/api',
  '/api/rest',
  '/api/files',
  '/api/mcp',
  '/api/openapi',
  '/api/healthz',
  '/api/errors',
  '/gateway',
  '/gateway/configuration',
  '/gateway/api-keys',
  '/gateway/reverse-proxy',
  '/gateway/logs',
  '/agent',
  '/agent/configuration',
  '/agent/local-logs',
  '/agent/key-rotation',
  '/operations',
  '/operations/deployment',
  '/operations/capability-changes',
  '/operations/uncertain-outcomes',
  '/operations/troubleshooting',
  '/reference',
  '/reference/cli',
  '/reference/environment-variables'
]

export default function sitemap() {
  return routes.map((route) => ({
    url: `${siteUrl}${route}`,
    changeFrequency: 'weekly',
    priority: route === '' ? 1 : 0.7
  }))
}
