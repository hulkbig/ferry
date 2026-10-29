import { defineConfig } from 'vitepress'
import { resolve } from 'node:path'

const entries = (en: boolean) => [
  { text: en ? 'Get started' : '开始使用', items: [
    { text: en ? 'Install with Docker' : 'Docker 安装', link: '/guide/docker' },
    { text: en ? 'Logical models' : '逻辑模型', link: '/guide/logical-models' },
    { text: 'Hermes Agent', link: '/guide/hermes' },
    { text: en ? 'Import accounts & keys' : '导入账号与 Key', link: '/guide/credential-import' },
    { text: 'AutoReset', link: '/guide/autoreset' },
    { text: en ? 'Codex usage protection' : 'Codex 额度保护', link: '/guide/codex-usage-protection' },
    { text: en ? 'FAQ' : '常见问题', link: '/guide/faq' },
    { text: en ? 'Troubleshooting' : '排障指南', link: '/guide/troubleshooting' },
  ] },
  { text: en ? 'Contribute' : '参与项目', items: [
    { text: en ? 'Contributing & PRs' : '贡献与 PR', link: '/contributing' },
    { text: en ? 'Development' : '开发指南', link: '/development' },
    { text: en ? 'Security' : '安全报告', link: '/security' },
  ] },
].map(group => ({ ...group, items: group.items.map(item => ({ ...item, link: (en ? '/en' : '') + item.link })) }))

export default defineConfig({
  title: 'Ferry',
  description: '为 AI 长任务提供稳定入口。Self-hosted routing and recovery for AI agents.',
  srcDir: './content',
  vite: { publicDir: resolve(import.meta.dirname, '../public'), build: { license: { fileName: 'assets/third-party-licenses.json' } } },
  base: process.env.FERRY_SITE_BASE || './',
  cleanUrls: false,
  lastUpdated: false,
  head: [['meta', { name: 'theme-color', content: '#242424' }]],
  locales: {
    root: { label: '简体中文', lang: 'zh-CN', title: 'Ferry', themeConfig: {
      nav: [{ text: '安装', link: '/guide/docker' }, { text: '文档', link: '/guide/' }, { text: 'GitHub', link: 'https://github.com/hulkbig/ferry' }],
      sidebar: entries(false), outline: { label: '本页内容', level: [2, 3] },
      docFooter: { prev: '上一篇', next: '下一篇' }, returnToTopLabel: '回到顶部', sidebarMenuLabel: '目录', darkModeSwitchLabel: '切换主题', langMenuLabel: '切换语言',
    } },
    en: { label: 'English', lang: 'en', title: 'Ferry', themeConfig: {
      nav: [{ text: 'Install', link: '/en/guide/docker' }, { text: 'Docs', link: '/en/guide/' }, { text: 'GitHub', link: 'https://github.com/hulkbig/ferry' }],
      sidebar: entries(true), outline: { label: 'On this page', level: [2, 3] },
    } },
  },
  themeConfig: {
    logo: { light: '/assets/brand.svg', dark: '/assets/brand-reverse.svg' },
    search: { provider: 'local', options: { locales: { root: { translations: { button: { buttonText: '搜索文档', buttonAriaLabel: '搜索文档' }, modal: { noResultsText: '未找到结果', resetButtonTitle: '清除', footer: { selectText: '选择', navigateText: '切换', closeText: '关闭' } } } } } } },
    footer: { message: 'Apache-2.0 · Third-party notices included', copyright: '© 2026 hulkbig' },
  },
})
