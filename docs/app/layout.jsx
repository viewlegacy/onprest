import { Footer, Layout, Navbar } from 'nextra-theme-docs'
import { Anchor, Head } from 'nextra/components'
import { GitHubIcon, GlobeIcon } from 'nextra/icons'
import { getPageMap } from 'nextra/page-map'
import 'nextra-theme-docs/style.css'

export const metadata = {
  title: {
    default: 'Onprest Docs',
    template: '%s | Onprest Docs'
  },
  description: 'Documentation for Onprest'
}

const navbar = (
  <Navbar
    logo={<b>Onprest</b>}
    projectLink="https://viewlegacy.com"
    projectIcon={<GlobeIcon height="24" aria-label="Company website" />}
  >
    <Anchor
      href="https://github.com/viewlegacy/onprest"
      aria-label="GitHub repository"
      title="GitHub repository"
    >
      <GitHubIcon height="24" />
    </Anchor>
  </Navbar>
)
const footer = <Footer>{new Date().getFullYear()} © Onprest.</Footer>

export default async function RootLayout({ children }) {
  return (
    <html lang="en" dir="ltr" suppressHydrationWarning>
      <Head />
      <body>
        <Layout
          navbar={navbar}
          pageMap={await getPageMap()}
          docsRepositoryBase="https://github.com/viewlegacy/onprest/tree/main/docs"
          footer={footer}
        >
          {children}
        </Layout>
      </body>
    </html>
  )
}
