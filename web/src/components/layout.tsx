import {
  AlertCircle,
  BarChart3,
  Blocks,
  Check,
  ChevronDown,
  ChevronRight,
  CreditCard,
  ExternalLink,
  FileKey2,
  FileText,
  Globe,
  Key,
  Layers,
  LayoutDashboard,
  Link2,
  LogOut,
  Menu,
  Package,
  Receipt,
  Rocket,
  ScrollText,
  Search,
  Settings,
  ShieldCheck,
  Tag,
  Ticket,
  User,
  Users,
} from "lucide-react"
import { useCallback, useMemo, useState } from "react"
import { Link, Navigate, Outlet, useLocation } from "react-router-dom"
import { ErrorBoundary } from "@/components/error-boundary"
import { ServiceUnavailableScreen } from "@/components/service-unavailable"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Sheet, SheetContent, SheetTrigger } from "@/components/ui/sheet"
import { useAuth } from "@/hooks/use-auth"
import { useSiteConfig } from "@/hooks/use-site-config"
import { useI18n } from "@/i18n"
import { cn } from "@/lib/utils"

export function AdminLayout() {
  const { user, loading, unavailable, logout, refetch } = useAuth()
  const { site_name, logo_url, attribution_text, attribution_url } = useSiteConfig()
  const { t, locale, setLocale } = useI18n()
  const [navOpen, setNavOpen] = useState(false)
  const [filterQuery, setFilterQuery] = useState("")

  type NavItem = { to: string; label: string; icon: React.ComponentType<{ className?: string }>; badge?: string }
  type NavGroup = { label: string; items: NavItem[] }

  const adminNav: (NavItem | NavGroup)[] = useMemo(
    () => [
      { to: "/admin", label: t("nav.dashboard"), icon: LayoutDashboard },
      {
        label: t("nav.catalog"),
        items: [
          { to: "/admin/products", label: t("nav.products"), icon: Package },
          { to: "/admin/plans", label: t("nav.plans"), icon: Layers },
          { to: "/admin/addons", label: t("nav.addons"), icon: Blocks },
          { to: "/admin/releases", label: t("nav.releases"), icon: Rocket },
        ],
      },
      {
        label: t("nav.licensing"),
        items: [
          { to: "/admin/licenses", label: t("nav.licenses"), icon: Key },
          { to: "/admin/customers", label: t("nav.customers"), icon: Users },
        ],
      },
      {
        label: t("admin.payments"),
        items: [
          { to: "/admin/payments/orders", label: t("admin.orders"), icon: Receipt },
          { to: "/admin/payments/invoices", label: t("nav.invoices"), icon: FileText },
          { to: "/admin/payments/unmatched", label: t("admin.unmatched"), icon: AlertCircle },
          { to: "/admin/payments/providers", label: t("admin.providers"), icon: CreditCard },
          { to: "/admin/payments/coupons", label: t("nav.coupons"), icon: Tag },
          { to: "/admin/payments/redemptions", label: t("nav.redemptions"), icon: Ticket },
          { to: "/admin/payments/reports", label: t("nav.reports"), icon: BarChart3 },
        ],
      },
      {
        label: t("nav.developer"),
        items: [
          { to: "/admin/api-keys", label: t("nav.apiKeys"), icon: FileKey2 },
          { to: "/admin/webhooks", label: t("nav.webhooks"), icon: Link2 },
        ],
      },
      {
        label: t("nav.insights"),
        items: [
          { to: "/admin/analytics", label: t("nav.analytics"), icon: BarChart3 },
          { to: "/admin/audit", label: t("nav.audit"), icon: ScrollText },
        ],
      },
    ],
    [t],
  )

  const location = useLocation()
  const settingsItem: NavItem = useMemo(
    () => ({ to: "/admin/settings", label: t("nav.settings"), icon: Settings }),
    [t],
  )

  const isActive = useCallback(
    (to: string) => (to === "/admin" ? location.pathname === "/admin" : location.pathname.startsWith(to)),
    [location.pathname],
  )

  // Find current active item and its group for breadcrumb trail
  const activeInfo = useMemo(() => {
    let currentGroup = ""
    let currentItem: NavItem = { to: "/admin", label: t("nav.dashboard"), icon: LayoutDashboard }

    if (location.pathname.startsWith("/admin/settings")) {
      return { group: t("nav.settings"), item: settingsItem }
    }

    const allItems: { group: string; item: NavItem }[] = []
    for (const entry of adminNav) {
      if ("to" in entry) {
        allItems.push({ group: "", item: entry })
      } else {
        for (const it of entry.items) {
          allItems.push({ group: entry.label, item: it })
        }
      }
    }

    const match = allItems.sort((a, b) => b.item.to.length - a.item.to.length).find((entry) => isActive(entry.item.to))

    if (match) {
      currentGroup = match.group
      currentItem = match.item
    }

    return { group: currentGroup, item: currentItem }
  }, [adminNav, isActive, location.pathname, settingsItem, t])

  // Filter items if user typed a query
  const filteredNav = useMemo(() => {
    const q = filterQuery.trim().toLowerCase()
    if (!q) return adminNav

    return adminNav
      .map((entry) => {
        if ("to" in entry) {
          return entry.label.toLowerCase().includes(q) ? entry : null
        }
        const matchingItems = entry.items.filter((it) => it.label.toLowerCase().includes(q))
        if (matchingItems.length === 0) return null
        return { label: entry.label, items: matchingItems }
      })
      .filter((entry): entry is NavItem | NavGroup => entry !== null)
  }, [adminNav, filterQuery])

  if (loading) return <LoadingScreen />
  if (!user && unavailable) return <ServiceUnavailableScreen onRetry={refetch} />
  if (!user) return <Navigate to="/login" replace />
  if (!user.is_admin) return <Navigate to="/portal" replace />

  const renderNavItem = (item: NavItem) => {
    const active = isActive(item.to)
    return (
      <Link key={item.to} to={item.to} onClick={() => setNavOpen(false)} className="block">
        <div
          className={cn(
            "group flex items-center justify-between rounded-md px-3 py-2 text-xs font-medium transition-colors",
            active
              ? "bg-primary/10 text-primary font-semibold shadow-xs"
              : "text-muted-foreground hover:bg-accent/60 hover:text-foreground",
          )}
        >
          <div className="flex items-center gap-2.5 truncate">
            <item.icon
              className={cn(
                "h-4 w-4 shrink-0 transition-colors",
                active ? "text-primary" : "text-muted-foreground/80 group-hover:text-foreground",
              )}
            />
            <span className="truncate">{item.label}</span>
          </div>
          {active && <span className="h-1.5 w-1.5 rounded-full bg-primary shrink-0" />}
        </div>
      </Link>
    )
  }

  const localeNames: Record<string, string> = {
    en: "English",
    zh: "中文",
    vi: "Tiếng Việt",
  }

  const sidebar = (
    <div className="flex flex-col h-full bg-card select-none">
      {/* Brand & Workspace Header */}
      <div className="p-3.5 pr-11 md:pr-3.5 border-b border-border/80 flex items-center justify-between gap-2.5">
        <Link to="/admin" className="flex items-center gap-2.5 min-w-0 group" onClick={() => setNavOpen(false)}>
          <div className="h-8 w-8 rounded-lg bg-primary/10 border border-primary/20 flex items-center justify-center shrink-0 group-hover:border-primary/40 transition-colors">
            <img src={logo_url || "/logo.svg"} alt="" className="h-5 w-5" />
          </div>
          <div className="min-w-0">
            <div className="font-semibold text-sm tracking-tight truncate leading-tight text-foreground">
              {site_name}
            </div>
            <div className="text-[11px] text-muted-foreground truncate leading-tight flex items-center gap-1.5 mt-0.5">
              <span className="h-1.5 w-1.5 rounded-full bg-emerald-500 inline-block shrink-0" />
              <span>License Engine</span>
            </div>
          </div>
        </Link>
        <span className="text-[11px] font-medium bg-primary/10 text-primary px-2 py-0.5 rounded-md border border-primary/20 shrink-0">
          Admin
        </span>
      </div>

      {/* Quick Search / Filter in Sidebar */}
      <div className="px-3 pt-2.5 pb-1">
        <div className="relative">
          <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-muted-foreground pointer-events-none" />
          <input
            type="text"
            value={filterQuery}
            onChange={(e) => setFilterQuery(e.target.value)}
            placeholder="Quick jump…"
            className="w-full h-8 pl-8 pr-2.5 text-xs bg-muted/60 border border-border/80 rounded-md focus:bg-background focus:outline-none focus:ring-1 focus:ring-primary focus:border-primary transition-colors text-foreground placeholder:text-muted-foreground/70"
          />
          {filterQuery && (
            <button
              type="button"
              onClick={() => setFilterQuery("")}
              className="absolute right-2 top-1/2 -translate-y-1/2 text-xs text-muted-foreground hover:text-foreground"
            >
              ×
            </button>
          )}
        </div>
      </div>

      {/* Navigation Groups */}
      <nav className="flex-1 px-2 py-2 space-y-3 overflow-y-auto">
        {filteredNav.map((entry, idx) => {
          if ("to" in entry) return <div key={entry.to}>{renderNavItem(entry)}</div>
          const group = entry as NavGroup
          return (
            <div key={group.label} className={cn(idx > 0 && "pt-1")}>
              <div className="px-3 py-1 text-[11px] font-medium text-muted-foreground/70 select-none">
                {group.label}
              </div>
              <div className="space-y-0.5 mt-0.5">{group.items.map(renderNavItem)}</div>
            </div>
          )
        })}
        {filteredNav.length === 0 && (
          <div className="px-3 py-4 text-xs text-center text-muted-foreground">No sections match your filter</div>
        )}
      </nav>

      {/* Settings & User Dock */}
      <div className="p-2 border-t border-border/80 space-y-1">
        <div>{renderNavItem(settingsItem)}</div>

        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button
              variant="ghost"
              className="w-full justify-start gap-2.5 px-2.5 py-2 h-auto text-left hover:bg-accent/60"
            >
              <div className="h-7 w-7 rounded-full bg-primary text-primary-foreground flex items-center justify-center text-xs font-semibold shrink-0 shadow-xs">
                {user.name?.charAt(0)?.toUpperCase() || user.email.charAt(0).toUpperCase()}
              </div>
              <div className="min-w-0 flex-1">
                <div className="text-xs font-medium text-foreground truncate">{user.name || user.email}</div>
                <div className="text-[10px] text-muted-foreground truncate">{user.email}</div>
              </div>
              <ChevronDown className="h-3.5 w-3.5 opacity-50 shrink-0" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-56">
            <div className="px-2 py-1.5">
              <div className="text-xs font-semibold text-foreground truncate">{user.name || "Administrator"}</div>
              <div className="text-[11px] text-muted-foreground truncate">{user.email}</div>
            </div>
            <DropdownMenuSeparator />
            <DropdownMenuItem asChild>
              <Link to="/portal" className="cursor-pointer">
                <ExternalLink className="h-3.5 w-3.5 mr-2 opacity-70" /> {t("nav.portal")}
              </Link>
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            {/* Language Selection */}
            <div className="px-2 py-1 text-[11px] font-medium text-muted-foreground flex items-center gap-1.5">
              <Globe className="h-3.5 w-3.5" /> Language
            </div>
            <DropdownMenuItem
              onClick={() => setLocale("en")}
              className={cn(
                "text-xs cursor-pointer flex items-center justify-between",
                locale === "en" && "font-semibold text-primary",
              )}
            >
              <span>{localeNames.en}</span>
              {locale === "en" && <Check className="h-3.5 w-3.5" />}
            </DropdownMenuItem>
            <DropdownMenuItem
              onClick={() => setLocale("zh")}
              className={cn(
                "text-xs cursor-pointer flex items-center justify-between",
                locale === "zh" && "font-semibold text-primary",
              )}
            >
              <span>{localeNames.zh}</span>
              {locale === "zh" && <Check className="h-3.5 w-3.5" />}
            </DropdownMenuItem>
            <DropdownMenuItem
              onClick={() => setLocale("vi")}
              className={cn(
                "text-xs cursor-pointer flex items-center justify-between",
                locale === "vi" && "font-semibold text-primary",
              )}
            >
              <span>{localeNames.vi}</span>
              {locale === "vi" && <Check className="h-3.5 w-3.5" />}
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem onClick={logout} className="text-destructive cursor-pointer">
              <LogOut className="h-3.5 w-3.5 mr-2" /> {t("nav.logout")}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>

        {/* AGPL Attribution */}
        <div className="px-2 pt-1 text-center">
          <a
            href={attribution_url}
            target="_blank"
            rel="noopener noreferrer"
            className="text-[10px] text-muted-foreground/60 hover:text-foreground transition-colors"
          >
            {attribution_text}
          </a>
        </div>
      </div>
    </div>
  )

  return (
    <div className="flex h-screen bg-background">
      {/* Permanent Desktop Sidebar */}
      <aside className="hidden w-64 shrink-0 border-r border-border/80 bg-card md:flex md:flex-col shadow-xs">
        {sidebar}
      </aside>

      {/* Main Content Column */}
      <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
        {/* Desktop Header App Bar */}
        <header className="hidden md:flex h-13 shrink-0 items-center justify-between border-b border-border/80 bg-card/60 px-6 backdrop-blur">
          {/* Breadcrumbs */}
          <div className="flex items-center gap-2 text-xs text-muted-foreground min-w-0">
            <Link to="/admin" className="hover:text-foreground transition-colors font-medium">
              Admin
            </Link>
            {activeInfo.group && (
              <>
                <ChevronRight className="h-3.5 w-3.5 shrink-0 opacity-40" />
                <span className="truncate">{activeInfo.group}</span>
              </>
            )}
            <ChevronRight className="h-3.5 w-3.5 shrink-0 opacity-40" />
            <span className="font-semibold text-foreground truncate">{activeInfo.item.label}</span>
          </div>

          {/* Quick Header Actions */}
          <div className="flex items-center gap-3">
            {/* Language Selector in Topbar */}
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-8 gap-1.5 text-xs text-muted-foreground hover:text-foreground"
                >
                  <Globe className="h-3.5 w-3.5" />
                  <span>{localeNames[locale]}</span>
                  <ChevronDown className="h-3 w-3 opacity-50" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-36">
                <DropdownMenuItem
                  onClick={() => setLocale("en")}
                  className={cn(
                    "text-xs cursor-pointer flex items-center justify-between",
                    locale === "en" && "font-semibold text-primary",
                  )}
                >
                  <span>{localeNames.en}</span>
                  {locale === "en" && <Check className="h-3.5 w-3.5" />}
                </DropdownMenuItem>
                <DropdownMenuItem
                  onClick={() => setLocale("zh")}
                  className={cn(
                    "text-xs cursor-pointer flex items-center justify-between",
                    locale === "zh" && "font-semibold text-primary",
                  )}
                >
                  <span>{localeNames.zh}</span>
                  {locale === "zh" && <Check className="h-3.5 w-3.5" />}
                </DropdownMenuItem>
                <DropdownMenuItem
                  onClick={() => setLocale("vi")}
                  className={cn(
                    "text-xs cursor-pointer flex items-center justify-between",
                    locale === "vi" && "font-semibold text-primary",
                  )}
                >
                  <span>{localeNames.vi}</span>
                  {locale === "vi" && <Check className="h-3.5 w-3.5" />}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>

            {/* Switch to Portal */}
            <Link to="/portal">
              <Button variant="outline" size="sm" className="h-8 gap-1.5 text-xs">
                <ShieldCheck className="h-3.5 w-3.5 text-primary" />
                <span>{t("nav.portal")}</span>
              </Button>
            </Link>
          </div>
        </header>

        {/* Mobile Header Bar */}
        <header className="flex h-13 shrink-0 items-center gap-2 border-b border-border/80 bg-card px-2 md:hidden">
          <Sheet open={navOpen} onOpenChange={setNavOpen}>
            <SheetTrigger asChild>
              <Button variant="ghost" size="icon" className="h-10 w-10 shrink-0" aria-label={t("nav.menu")}>
                <Menu className="h-5 w-5" />
              </Button>
            </SheetTrigger>
            <SheetContent side="left" className="p-0 w-72" label={t("nav.menu")}>
              {sidebar}
            </SheetContent>
          </Sheet>

          <Link to="/admin" className="shrink-0 flex items-center gap-2" aria-label={site_name}>
            <img src={logo_url || "/logo.svg"} alt="" className="h-6 w-6" />
          </Link>

          <span className="min-w-0 flex-1 truncate font-semibold text-sm tracking-tight">{activeInfo.item.label}</span>

          <Link to="/portal">
            <Button variant="ghost" size="sm" className="text-xs h-8">
              {t("nav.portal")}
            </Button>
          </Link>
        </header>

        {/* Main Routed Content Area */}
        <main className="flex-1 overflow-auto bg-background/50">
          <div className="p-4 md:p-6 lg:p-8 max-w-7xl">
            <ErrorBoundary>
              <Outlet />
            </ErrorBoundary>
          </div>
        </main>

        {/* Mobile Attribution Bar */}
        <div className="shrink-0 border-t border-border/80 bg-card px-4 py-1.5 text-center md:hidden">
          <a
            href={attribution_url}
            target="_blank"
            rel="noopener noreferrer"
            className="text-[10px] text-muted-foreground/60 hover:text-foreground transition-colors"
          >
            {attribution_text}
          </a>
        </div>
      </div>
    </div>
  )
}

export function PortalLayout() {
  const { user, loading, unavailable, logout, refetch } = useAuth()
  const { site_name, logo_url, attribution_text, attribution_url } = useSiteConfig()
  const { t, locale, setLocale } = useI18n()

  const portalNav = useMemo(
    () => [
      { to: "/portal", label: t("nav.licenses"), icon: Key },
      { to: "/portal/payments", label: t("nav.portalPayments"), icon: Receipt },
      { to: "/portal/invoices", label: t("nav.invoices"), icon: FileText },
      { to: "/portal/billing-profile", label: t("nav.billingProfile"), icon: CreditCard },
      { to: "/portal/account", label: t("nav.settings"), icon: User },
    ],
    [t],
  )
  const location = useLocation()

  if (loading) return <LoadingScreen />
  if (!user && unavailable) return <ServiceUnavailableScreen onRetry={refetch} />
  if (!user) return <Navigate to="/login" replace />

  const localeNames: Record<string, string> = {
    en: "English",
    zh: "中文",
    vi: "Tiếng Việt",
  }

  return (
    <div className="min-h-screen bg-background flex flex-col">
      <header className="border-b border-border/80 bg-card/80 backdrop-blur sticky top-0 z-40">
        <div className="max-w-6xl mx-auto flex items-center justify-between h-14 px-4">
          <Link to="/portal" className="flex items-center gap-2.5 font-bold text-base tracking-tight group">
            <div className="h-7 w-7 rounded-md bg-primary/10 border border-primary/20 flex items-center justify-center shrink-0">
              <img src={logo_url || "/logo.svg"} alt="" className="h-4.5 w-4.5" />
            </div>
            <span className="text-foreground">{site_name}</span>
            <span className="text-[10px] font-medium bg-muted text-muted-foreground px-1.5 py-0.5 rounded">
              Customer Portal
            </span>
          </Link>

          <div className="flex items-center gap-3">
            {/* Language Selector */}
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-8 gap-1.5 text-xs text-muted-foreground hover:text-foreground"
                >
                  <Globe className="h-3.5 w-3.5" />
                  <span className="hidden sm:inline">{localeNames[locale]}</span>
                  <ChevronDown className="h-3 w-3 opacity-50" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-36">
                <DropdownMenuItem
                  onClick={() => setLocale("en")}
                  className={cn(
                    "text-xs cursor-pointer flex items-center justify-between",
                    locale === "en" && "font-semibold text-primary",
                  )}
                >
                  <span>{localeNames.en}</span>
                  {locale === "en" && <Check className="h-3.5 w-3.5" />}
                </DropdownMenuItem>
                <DropdownMenuItem
                  onClick={() => setLocale("zh")}
                  className={cn(
                    "text-xs cursor-pointer flex items-center justify-between",
                    locale === "zh" && "font-semibold text-primary",
                  )}
                >
                  <span>{localeNames.zh}</span>
                  {locale === "zh" && <Check className="h-3.5 w-3.5" />}
                </DropdownMenuItem>
                <DropdownMenuItem
                  onClick={() => setLocale("vi")}
                  className={cn(
                    "text-xs cursor-pointer flex items-center justify-between",
                    locale === "vi" && "font-semibold text-primary",
                  )}
                >
                  <span>{localeNames.vi}</span>
                  {locale === "vi" && <Check className="h-3.5 w-3.5" />}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>

            {user.is_admin && (
              <Link to="/admin">
                <Button variant="outline" size="sm" className="h-8 text-xs gap-1.5">
                  <ShieldCheck className="h-3.5 w-3.5 text-primary" />
                  <span>Admin Panel</span>
                </Button>
              </Link>
            )}

            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="sm" className="h-8 gap-2 px-2">
                  <div className="h-6 w-6 rounded-full bg-primary text-primary-foreground flex items-center justify-center text-xs font-medium">
                    {user.name?.charAt(0)?.toUpperCase() || user.email.charAt(0).toUpperCase()}
                  </div>
                  <span className="text-xs max-w-32 truncate hidden sm:inline">{user.name || user.email}</span>
                  <ChevronDown className="h-3 w-3 opacity-50" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-48">
                <div className="px-2 py-1.5">
                  <div className="text-xs font-semibold text-foreground truncate">{user.name || "Customer"}</div>
                  <div className="text-[11px] text-muted-foreground truncate">{user.email}</div>
                </div>
                <DropdownMenuSeparator />
                <DropdownMenuItem onClick={logout} className="text-destructive cursor-pointer">
                  <LogOut className="h-3.5 w-3.5 mr-2" /> {t("nav.logout")}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        </div>

        {/* Portal Pill Tab Navigation */}
        <div className="max-w-6xl mx-auto px-4">
          <nav className="flex gap-1 overflow-x-auto py-1">
            {portalNav.map((item) => {
              const active =
                item.to === "/portal" ? location.pathname === "/portal" : location.pathname.startsWith(item.to)
              return (
                <Link key={item.to} to={item.to} className="shrink-0">
                  <div
                    className={cn(
                      "flex items-center gap-2 px-3 py-1.5 text-xs font-medium rounded-md transition-colors",
                      active
                        ? "bg-primary text-primary-foreground shadow-xs font-semibold"
                        : "text-muted-foreground hover:bg-accent/60 hover:text-foreground",
                    )}
                  >
                    <item.icon className="h-3.5 w-3.5" />
                    <span>{item.label}</span>
                  </div>
                </Link>
              )
            })}
          </nav>
        </div>
      </header>

      <main className="max-w-6xl mx-auto p-4 md:p-6 lg:p-8 flex-1 w-full">
        <ErrorBoundary>
          <Outlet />
        </ErrorBoundary>
      </main>

      {/* Attribution required by AGPL v3 Section 7(b) */}
      <footer className="border-t border-border/80 py-3 text-center bg-card/40">
        <a
          href={attribution_url}
          target="_blank"
          rel="noopener noreferrer"
          className="text-xs text-muted-foreground/60 hover:text-foreground transition-colors"
        >
          {attribution_text}
        </a>
      </footer>
    </div>
  )
}

function LoadingScreen() {
  return (
    <div className="flex items-center justify-center h-screen bg-background">
      <div className="animate-spin motion-reduce:hidden h-8 w-8 border-2 border-primary border-t-transparent rounded-full" />
      <span className="sr-only">Loading…</span>
    </div>
  )
}
