import { Footer, Layout, Navbar } from 'nextra-theme-docs'
import { Anchor, Head } from 'nextra/components'
import { GitHubIcon, GlobeIcon } from 'nextra/icons'
import { getPageMap } from 'nextra/page-map'
import './logo.css'
import 'nextra-theme-docs/style.css'

const siteUrl = 'https://docs.onprest.viewlegacy.com'

export const metadata = {
  metadataBase: new URL(siteUrl),
  title: {
    default: 'Onprest Docs',
    template: '%s | Onprest Docs'
  },
  description:
    'Documentation for Onprest, an open source gateway and agent for exposing on-premises databases through REST API and MCP.',
  applicationName: 'Onprest Docs',
  authors: [{ name: 'ViewLegacy LLC', url: 'https://viewlegacy.com' }],
  creator: 'ViewLegacy LLC',
  publisher: 'ViewLegacy LLC',
  openGraph: {
    type: 'website',
    url: siteUrl,
    siteName: 'Onprest Docs',
    title: 'Onprest Docs',
    description:
      'Documentation for Onprest, an open source gateway and agent for exposing on-premises databases through REST API and MCP.'
  },
  twitter: {
    card: 'summary',
    title: 'Onprest Docs',
    description:
      'Documentation for Onprest, an open source gateway and agent for exposing on-premises databases through REST API and MCP.'
  }
}

const logo = (
  <span className="onprest-logo" aria-label="Onprest">
    <img
      className="onprest-logo__image onprest-logo__image--light"
      src="/logo_black.png"
      alt=""
      width="154"
      height="24"
      aria-hidden="true"
    />
    <img
      className="onprest-logo__image onprest-logo__image--dark"
      src="/logo_white.png"
      alt=""
      width="154"
      height="24"
      aria-hidden="true"
    />
  </span>
)

const navbar = (
  <Navbar
    logo={logo}
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
const footer = <Footer>{new Date().getFullYear()} © ViewLegacy LLC.</Footer>

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
