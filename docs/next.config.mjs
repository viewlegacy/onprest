import nextra from 'nextra'

const withNextra = nextra({})

const movedPages = [
  ['/agent/capability-yaml', '/capabilities'],
  ['/databases', '/capabilities/databases'],
  ['/databases/:driver(postgres|mysql|sqlserver|oracle)', '/capabilities/databases/:driver'],
  ['/reference/logging', '/gateway/logs'],
  ['/operations/mutation-reconciliation', '/operations/uncertain-outcomes'],
  ['/operations/release-gate', '/operations/deployment'],
  ['/reference/test-commands', 'https://github.com/viewlegacy/onprest/blob/main/CONTRIBUTING.md']
]

export default withNextra({
  async redirects() {
    return movedPages.map(([source, destination]) => ({ source, destination, permanent: true }))
  }
})
