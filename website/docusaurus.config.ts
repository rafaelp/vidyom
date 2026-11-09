import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';
const config: Config = {
  title: 'Vidyom',
  tagline: 'Preserve · Discover · Transform · Share',
  favicon: 'img/favicon.ico',
  url: process.env.DOCS_SITE_URL || 'https://www.vidyom.org',
  baseUrl: process.env.DOCS_BASE_URL || '/',
  organizationName: 'rafaelp',
  projectName: 'vidyom',
  onBrokenLinks: 'throw',
  onBrokenMarkdownLinks: 'warn',
  i18n: {defaultLocale: 'en', locales: ['en']},
  presets: [['classic', {
    docs: {sidebarPath: './sidebars.ts', editUrl: 'https://github.com/rafaelp/vidyom/tree/main/website/', showLastUpdateTime: false},
    blog: false,
    theme: {customCss: './src/css/custom.css'},
  } satisfies Preset.Options]],
  themeConfig: {
    image: 'img/social-card.svg',
    navbar: {
      logo: {alt: 'Vidyom', src: 'img/vidyom-logo.png', srcDark: 'img/vidyom-logo-dark.png'},
      items: [
        {type: 'docSidebar', sidebarId: 'docsSidebar', position: 'left', label: 'Documentation'},
        {to: '/docs/architecture/overview', label: 'Architecture', position: 'left'},
        {to: '/docs/workflows/ingest-original', label: 'Workflows', position: 'left'},
        {href: 'https://github.com/rafaelp/vidyom', label: 'GitHub', position: 'right'},
      ],
    },
    footer: {style: 'dark', links: [
      {title: 'Explore', items: [{label: 'Getting started',to:'/docs/intro'},{label:'CLI reference',to:'/docs/reference/cli'}]},
      {title: 'Project', items: [{label:'Contributing',to:'/docs/contributing/guide'},{label:'GitHub',href:'https://github.com/rafaelp/vidyom'}]},
      {title: 'Origins', items: [{label:'Originally built for MapaDeva',href:'https://www.mapadeva.com.br/'}]},
    ], copyright: `© ${new Date().getFullYear()} Vidyom contributors. Documentation is a design specification.`},
    prism: {additionalLanguages: ['bash','json','yaml','go']},
    colorMode: {defaultMode:'dark', respectPrefersColorScheme:true},
  } satisfies Preset.ThemeConfig,
};
export default config;
