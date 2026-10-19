/**
 * The `bluearchive` theme: this project's own blog theme.
 *
 * A Blue Archive *inspired* personal blog — ice blue, white, soft shadows, very
 * rounded shapes — written as ordinary Astro components. The visual reference is
 * `Alittfre/vitepress-theme-bluearchive`; nothing from it is a runtime
 * dependency, and nothing from a VitePress theme is imported here. It is a look,
 * not a codebase.
 *
 * ARCHITECTURE.md §44: a theme is a manifest plus two component trees. This file
 * is the only thing the registry imports, so a theme can never be reached by path,
 * by a glob or by an id that came off the network.
 *
 * ARCHITECTURE.md §45/ID-19: notice what is *not* here. No API client, no
 * filesystem access, no session handling, no password or CSRF logic. The one piece
 * of JavaScript in this theme is the header's mobile menu, it lives inside the
 * component that owns the markup, and it cannot make a request. Everything a
 * visitor can *do* — signing in, saving a post, posting a comment — is a core
 * script at a fixed URL that this theme does not own and cannot replace.
 */
import type { ThemeDefinition } from '../../theme-system/contract';

// Public
import PublicLayout from './public/layouts/PublicLayout.astro';
import HomeView from './public/views/HomeView.astro';
import ListingView from './public/views/ListingView.astro';
import PostView from './public/views/PostView.astro';
import PageView from './public/views/PageView.astro';
import NotFoundView from './public/views/NotFoundView.astro';
import PostCard from './public/components/PostCard.astro';
import CommentSection from './public/components/CommentSection.astro';
import CommentList from './public/components/CommentList.astro';

// Admin
import AdminLayout from './admin/layouts/AdminLayout.astro';
import LoginView from './admin/views/LoginView.astro';
import DashboardView from './admin/views/DashboardView.astro';
import PostsView from './admin/views/PostsView.astro';
import PostEditorView from './admin/views/PostEditorView.astro';
import PagesView from './admin/views/PagesView.astro';
import PageEditorView from './admin/views/PageEditorView.astro';
import CommentsView from './admin/views/CommentsView.astro';
import MediaView from './admin/views/MediaView.astro';
import SettingsView from './admin/views/SettingsView.astro';
import CustomCodeView from './admin/views/CustomCodeView.astro';
import CustomCodeEditorView from './admin/views/CustomCodeEditorView.astro';
import MarkdownView from './admin/views/MarkdownView.astro';
import BackupView from './admin/views/BackupView.astro';
import MarkdownEditorView from './admin/views/MarkdownEditorView.astro';
import AdminsView from './admin/views/AdminsView.astro';
import AdminDetailView from './admin/views/AdminDetailView.astro';
import AdminNotice from './admin/components/AdminNotice.astro';
import AdminTable from './admin/components/AdminTable.astro';

export const blueArchiveTheme: ThemeDefinition = {
  id: 'bluearchive',
  name: 'Blue Archive',
  version: '1.0.0',

  /**
   * Language for the shared core scripts (/cms.js, /comments.js).
   *
   * ARCHITECTURE.md §19: these scripts are the same file for every theme and this
   * theme does not own them. A notice is still presentation though, so the words come
   * from here — otherwise a Chinese admin would read "Saved." after every save.
   *
   * English technical nouns are kept as-is on purpose: translating "WebP" or "Markdown"
   * produces text an admin then has to translate back.
   */
  notices: {
    saved: '已保存。',
    settingsSaved: '设置已保存。',
    savedReload: '已保存。刷新博客即可看到变化。',
    selected: '已选择。',
    uploaded: '上传完成。',
    uploadedInserted: '已上传并插入。',
    altSaved: '替代文本已保存。',
    copied: '已复制。',
    copyNeedsHTTPS: '复制功能需要 https —— 请手动选中并复制该值。',
    copyFailed: '无法自动复制 —— 请手动选中并复制该值。',
    nothingToCopy: '没有可复制的内容。',
    chooseFile: '请先选择文件。',
    serverUnreachable: '无法连接到服务器。',
    deleted: '已删除。',
    confirmDelete: '确定要执行这个删除操作吗?此操作不可撤销。',
    confirmClearCache: '确定清空全部已生成的 WebP 副本?原始文件不受影响,下次请求会重新生成。',
    confirmRebuildUsage: '确定重建媒体引用索引?该索引由 content/ 扫描得出,重建后会覆盖现有结果。',
    confirmLeaveEditor: '当前文件有未保存的修改,确定要离开吗?',
    commentPosted: '评论已提交。',
    commentHeld: '评论已提交,等待审核后显示。',
    saveSettingsFailed: '设置保存失败。',
    saveAltFailed: '替代文本保存失败。',
    clearCacheFailed: '清空缓存失败。',
    rebuildUsageFailed: '重建索引失败。',
    deleteFailed: '删除失败。',
    updateFailed: '更新失败。',
    homeNav: '首页',
    'colorScheme.toLight': '切换到浅色配色',
    'colorScheme.toDark': '切换到深色配色',
    'colorScheme.follow': '跟随系统配色',
    'listing.latest': '最新文章',
    'listing.empty': '还没有文章。请往 content/posts/ 里添加一个 Markdown 文件。',
    'listing.tagged': '标签为“{tag}”的文章',
    'listing.emptyTagged': '没有该标签下的文章。',
    backupInitialized: '备份仓库已初始化。',
    backupCommitted: '备份已创建。',
    backupNothingToCommit: '没有内容变化,无需备份。',
    backupFailed: '备份失败。',
    backupInitializeFailed: '初始化备份仓库失败。',
    // 管理员管理与 GitHub 账号绑定(ARCHITECTURE.md §36)。
    adminCreated: '管理员已创建。',
    adminCreateFailed: '创建管理员失败。',
    adminStatusChanged: '管理员状态已更新。',
    adminStatusFailed: '更新管理员状态失败。',
    passwordChanged: '密码已修改,该账户的其他会话已失效。',
    changePasswordFailed: '修改密码失败。',
    passwordReset: '密码已重置,该账户的所有会话已失效。',
    resetPasswordFailed: '重置密码失败。',
    githubBindStarted: '正在前往 GitHub 授权…',
    githubBindFailed: '无法开始 GitHub 绑定。',
    githubUnbound: '已解绑,本地保存的授权凭据已删除。',
    githubUnbindFailed: '解绑失败。',
    githubConfigSaved: 'GitHub OAuth 配置已保存。',
    githubConfigFailed: '保存 GitHub OAuth 配置失败。',
    // 首次初始化(ID-71):随机生成的凭据只显示一次。
    setupCopied: '已复制到剪贴板。',
    setupCopyFailed: '浏览器拒绝访问剪贴板,请手动选中复制。',
  },

  /**
   * Admin screen names, keyed by the English literal each core page passes to
   * adminSeo(). A new core screen therefore shows an English tab until it is added
   * here — visible, and fixed by adding one line.
   */
  adminTitles: {
    Dashboard: '仪表盘',
    Posts: '文章',
    Pages: '页面',
    Media: '媒体库',
    Comments: '评论',
    Settings: '设置',
    'Custom code': 'Custom CSS && JS',
    'Custom assets': '自定义资源',
    Markdown: '样式模板',
    'Markdown editor': 'Markdown 编辑器',
    Backup: '备份',
    Admins: '管理员',
    'New post': '写文章',
    'New page': '新建页面',
    Edit: '编辑',
    Sign: '登录',
    'Sign in': '登录',
    'Create the administrator': '创建管理员',
    'Posts tagged': '标签为“{tag}”的文章',
    adminSuffix: '后台',
  },

  public: {
    Layout: PublicLayout,
    HomeView,
    ListingView,
    PostView,
    PageView,
    NotFoundView,
    PostCard,
    CommentSection,
    CommentList,
  },

  admin: {
    Layout: AdminLayout,
    LoginView,
    DashboardView,
    PostsView,
    PostEditorView,
    PagesView,
    PageEditorView,
    CommentsView,
    MediaView,
    SettingsView,
    CustomCodeView,
    CustomCodeEditorView,
    MarkdownView,
    BackupView,
    MarkdownEditorView,
    AdminsView,
    AdminDetailView,
    Notice: AdminNotice,
    Table: AdminTable,
  },
};

export default blueArchiveTheme;
