import { defineConfig } from 'vitepress'

const repo = 'https://github.com/sivi-shahab/KarhutlaSQL'

const start = [
  { text: 'Why KarhutlaSQL?', link: '/guide/why' },
  { text: 'Introduction', link: '/guide/introduction' },
  { text: 'Quick start (5 minutes)', link: '/guide/quick-start' },
  { text: 'Core concepts', link: '/guide/concepts' },
  { text: 'Installation', link: '/install' },
]

const tutorials = [
  { text: '1. Your first query (CLI)', link: '/tutorials/01-first-query' },
  { text: '2. Python, pandas & bulk ingest', link: '/tutorials/02-python' },
  { text: '3. JDBC, DBeaver & apps', link: '/tutorials/03-jdbc-bi-tools' },
  { text: '4. Masking & row-level security', link: '/tutorials/04-masking-rls' },
  { text: '5. SSO login with Keycloak', link: '/tutorials/05-sso' },
  { text: '6. Signed reports', link: '/tutorials/06-signed-reports' },
  { text: '7. Data quality with Great Expectations', link: '/tutorials/07-data-quality' },
  { text: '8. AI agents via MCP', link: '/tutorials/08-ai-agents-mcp' },
  { text: '9. Monitoring & session control', link: '/tutorials/09-monitoring' },
  { text: '10. Multi-instance DuckLake', link: '/tutorials/10-ducklake' },
]

const guides = [
  { text: 'CLI client', link: '/client' },
  { text: 'PostgreSQL protocol', link: '/postgres' },
  { text: 'HTTP query API (Arrow)', link: '/http-api' },
  { text: 'Authentication', link: '/auth' },
  { text: 'Policy & governance', link: '/policy' },
  { text: 'Monitoring', link: '/monitoring' },
  { text: 'Signed report runs', link: '/reports' },
  { text: 'Scheduled jobs & materialized tables', link: '/jobs' },
  { text: 'Data quality', link: '/data-quality' },
  { text: 'Unity Catalog', link: '/catalog' },
  { text: 'MCP server (AI agents)', link: '/mcp' },
  { text: 'DuckLake multi-writer', link: '/ducklake' },
  { text: 'Backup, migrations & keys', link: '/operations' },
  { text: 'Kubernetes (Helm)', link: '/deploy-k8s' },
]

const reference = [
  { text: 'Server configuration', link: '/config' },
  { text: 'SQL functions & DoActions', link: '/reference/sql' },
  { text: 'Release procedure', link: '/release' },
]

const architecture = [
  { text: 'Architecture diagram', link: '/architecture' },
  { text: 'Research: features & compliance', link: '/research/fitur-lanjutan' },
]

const sidebar = [
  { text: 'Getting started', items: start },
  { text: 'Tutorials', items: tutorials },
  { text: 'Feature guides', items: guides },
  { text: 'Reference', items: reference },
  { text: 'Architecture', items: architecture },
]

export default defineConfig({
  lang: 'en-US',
  title: 'KarhutlaSQL',
  description: 'Arrow Flight SQL on DuckDB — an analytical SQL server with server-side governance',
  base: '/KarhutlaSQL/',
  cleanUrls: true,
  ignoreDeadLinks: 'localhostLinks',
  srcExclude: ['README.md'],
  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: '/KarhutlaSQL/logo.svg' }],
    ['meta', { name: 'theme-color', content: '#e8590c' }],
  ],
  themeConfig: {
    logo: '/logo.svg',
    nav: [
      { text: 'Guide', link: '/guide/why', activeMatch: '/guide/' },
      { text: 'Tutorials', link: '/tutorials/01-first-query', activeMatch: '/tutorials/' },
      { text: 'Features', items: guides },
      { text: 'Reference', items: reference },
      { text: 'Diagram', link: 'https://sivi-shahab.github.io/KarhutlaSQL/diagram/architecture.html' },
    ],
    sidebar,
    socialLinks: [{ icon: 'github', link: repo }],
    search: { provider: 'local' },
    editLink: { pattern: `${repo}/edit/main/docs/:path`, text: 'Edit this page on GitHub' },
    outline: { level: [2, 3] },
    footer: {
      message: 'Released under the Apache 2.0 License.',
      copyright: 'KarhutlaSQL',
    },
  },
})
