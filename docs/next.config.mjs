import nextra from 'nextra'

const withNextra = nextra({})

// Pages that moved when the documentation was reorganized around executors.
const movedPages = [
  ['/agent/capability-yaml', '/capabilities'],
  ['/databases', '/capabilities/databases'],
  ['/databases/:driver(postgres|mysql|sqlserver|oracle)', '/capabilities/databases/:driver'],
  ['/reference/logging', '/gateway/logs'],
  ['/operations/mutation-reconciliation', '/operations/uncertain-outcomes']
]

export default withNextra({
  async redirects() {
    return movedPages.map(([source, destination]) => ({ source, destination, permanent: true }))
  }
})
