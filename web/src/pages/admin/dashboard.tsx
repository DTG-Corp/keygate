import { useQuery } from "@tanstack/react-query"
import { Activity, ArrowUpCircle, ChevronRight, Key, Package, Sparkles, Users } from "lucide-react"
import { Link } from "react-router-dom"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  DataTable,
  DataTableBody,
  DataTableCell,
  DataTableHead,
  DataTableHeader,
  DataTableRow,
} from "@/components/ui/data-table"
import { type TranslationKeys, useI18n } from "@/i18n"
import { admin } from "@/lib/api"
import { formatDate, statusColor } from "@/lib/utils"

export default function DashboardPage() {
  const { t } = useI18n()
  const { data: stats, isLoading } = useQuery({ queryKey: ["admin", "stats"], queryFn: admin.stats })
  const { data: updateData } = useQuery({
    queryKey: ["admin", "update-check"],
    queryFn: admin.checkUpdate,
    staleTime: 60 * 60 * 1000,
  })

  if (isLoading || !stats) {
    return (
      <div className="motion-safe:animate-pulse space-y-4">
        <div className="h-28 bg-muted rounded-lg" />
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
          <div className="h-28 bg-muted rounded-lg" />
          <div className="h-28 bg-muted rounded-lg" />
          <div className="h-28 bg-muted rounded-lg" />
          <div className="h-28 bg-muted rounded-lg" />
        </div>
        <div className="h-64 bg-muted rounded-lg" />
      </div>
    )
  }

  const cards = [
    {
      label: t("dashboard.totalLicenses"),
      value: stats.total_licenses,
      icon: Key,
      color: "text-primary",
      bg: "bg-primary/10",
      href: "/admin/licenses",
    },
    {
      label: t("dashboard.activeLicenses"),
      value: stats.active_licenses,
      icon: Activity,
      color: "text-emerald-600",
      bg: "bg-emerald-500/10",
      href: "/admin/licenses?status=active",
    },
    {
      label: t("dashboard.totalActivations"),
      value: stats.total_activations,
      icon: Users,
      color: "text-primary",
      bg: "bg-primary/10",
      href: "/admin/customers",
    },
    {
      label: t("dashboard.products"),
      value: stats.total_products,
      icon: Package,
      color: "text-amber-600",
      bg: "bg-amber-500/10",
      href: "/admin/products",
    },
  ]

  return (
    <div className="space-y-6">
      {/* Page Header */}
      <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-4 pb-2 border-b border-border/60">
        <div>
          <h1 className="text-xl font-bold tracking-tight text-foreground">{t("dashboard.title")}</h1>
          <p className="text-xs text-muted-foreground mt-0.5">{t("dashboard.subtitle")}</p>
        </div>
        <div className="flex items-center gap-2">
          <Link to="/admin/licenses">
            <Button size="sm" className="h-8 text-xs gap-1.5 shadow-xs">
              <Key className="h-3.5 w-3.5" />
              <span>{t("nav.licenses")}</span>
            </Button>
          </Link>
          <Link to="/admin/products">
            <Button size="sm" variant="outline" className="h-8 text-xs gap-1.5">
              <Package className="h-3.5 w-3.5" />
              <span>{t("nav.products")}</span>
            </Button>
          </Link>
        </div>
      </div>

      {/* Update Available Notification Banner */}
      {updateData?.available && (
        <div className="flex flex-col sm:flex-row sm:items-center justify-between rounded-lg border border-primary/20 bg-primary/5 px-4 py-3 gap-3">
          <div className="flex items-start sm:items-center gap-3">
            <div className="h-8 w-8 rounded-md bg-primary/10 flex items-center justify-center shrink-0 text-primary">
              <ArrowUpCircle className="h-5 w-5" />
            </div>
            <div>
              <p className="text-xs font-semibold text-foreground flex items-center gap-1.5">
                <span>{t("dashboard.updateAvailable", { version: updateData.latest_version })}</span>
                <span className="text-[10px] bg-primary/20 text-primary px-1.5 py-0.2 rounded font-medium">new</span>
              </p>
              {updateData.update_command && (
                <code className="text-[11px] text-muted-foreground font-mono mt-0.5 block bg-muted/60 px-2 py-0.5 rounded">
                  {updateData.update_command}
                </code>
              )}
            </div>
          </div>
          <div className="flex items-center gap-2 shrink-0">
            <Link to="/admin/settings?tab=system">
              <Button size="sm" variant="outline" className="h-7 text-xs">
                {t("dashboard.viewUpdate")}
              </Button>
            </Link>
            {updateData.release_url && (
              <a href={updateData.release_url} target="_blank" rel="noopener noreferrer">
                <Button size="sm" className="h-7 text-xs">
                  {t("dashboard.releaseNotes")}
                </Button>
              </a>
            )}
          </div>
        </div>
      )}

      {/* KPI Metrics Cards */}
      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        {cards.map((c) => (
          <Link key={c.label} to={c.href} className="group block focus-visible:outline-none">
            <Card className="transition-colors group-hover:border-primary/40 group-focus-visible:ring-2 group-focus-visible:ring-primary shadow-xs">
              <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
                <CardTitle className="text-xs font-medium text-muted-foreground group-hover:text-foreground transition-colors">
                  {c.label}
                </CardTitle>
                <div className={`h-7 w-7 rounded-md ${c.bg} flex items-center justify-center shrink-0`}>
                  <c.icon className={`h-4 w-4 ${c.color}`} />
                </div>
              </CardHeader>
              <CardContent>
                <div className="text-2xl font-bold tracking-tight num">{c.value}</div>
                <div className="text-[11px] text-muted-foreground flex items-center gap-1 mt-1 opacity-80 group-hover:opacity-100 transition-opacity">
                  <span>View details</span>
                  <ChevronRight className="h-3 w-3 group-hover:translate-x-0.5 transition-transform" />
                </div>
              </CardContent>
            </Card>
          </Link>
        ))}
      </div>

      {/* Status Breakdown Section */}
      {Object.keys(stats.by_status).length > 0 && (
        <Card className="shadow-xs">
          <CardHeader className="pb-3 border-b border-border/50">
            <div className="flex items-center justify-between">
              <CardTitle className="text-sm font-semibold">{t("dashboard.statusDistribution")}</CardTitle>
              <div className="text-xs text-muted-foreground flex items-center gap-1.5">
                <Sparkles className="h-3.5 w-3.5 text-primary" />
                <span>Live status sync</span>
              </div>
            </div>
          </CardHeader>
          <CardContent className="pt-4">
            <div className="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-6 gap-3">
              {Object.entries(stats.by_status).map(([status, count]) => (
                <div
                  key={status}
                  className="rounded-lg border border-border/60 bg-muted/30 p-2.5 flex flex-col justify-between hover:bg-muted/50 transition-colors"
                >
                  <div className="text-[11px] font-medium text-muted-foreground truncate">
                    <Badge className={statusColor(status)}>{t(`status.${status}` as TranslationKeys)}</Badge>
                  </div>
                  <div className="text-lg font-bold mt-2 num text-foreground">{count}</div>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      )}

      {/* Recent Licenses Table */}
      {stats.recent_licenses?.length > 0 && (
        <Card className="shadow-xs">
          <CardHeader className="pb-3 border-b border-border/50 flex flex-row items-center justify-between">
            <div>
              <CardTitle className="text-sm font-semibold">{t("dashboard.recentLicenses")}</CardTitle>
              <p className="text-xs text-muted-foreground mt-0.5">Recently issued and updated software licenses</p>
            </div>
            <Link to="/admin/licenses">
              <Button
                variant="ghost"
                size="sm"
                className="h-7 text-xs gap-1 text-muted-foreground hover:text-foreground"
              >
                <span>View all</span>
                <ChevronRight className="h-3.5 w-3.5" />
              </Button>
            </Link>
          </CardHeader>
          <CardContent className="pt-0 px-0">
            <DataTable className="border-0 rounded-none">
              <DataTableHeader>
                <DataTableRow>
                  <DataTableHead>{t("common.email")}</DataTableHead>
                  <DataTableHead>{t("common.product")}</DataTableHead>
                  <DataTableHead>{t("common.plan")}</DataTableHead>
                  <DataTableHead>{t("common.status")}</DataTableHead>
                  <DataTableHead className="text-right">{t("common.created")}</DataTableHead>
                </DataTableRow>
              </DataTableHeader>
              <DataTableBody>
                {stats.recent_licenses.map((lic) => (
                  <DataTableRow key={lic.id} className="hover:bg-muted/40 transition-colors">
                    <DataTableCell>
                      <Link
                        to={`/admin/licenses?id=${lic.id}`}
                        className="font-medium text-xs text-foreground hover:text-primary transition-colors hover:underline"
                      >
                        {lic.email}
                      </Link>
                    </DataTableCell>
                    <DataTableCell className="text-xs text-muted-foreground font-mono">
                      {lic.product?.name || "N/A"}
                    </DataTableCell>
                    <DataTableCell className="text-xs text-muted-foreground">{lic.plan?.name || "N/A"}</DataTableCell>
                    <DataTableCell>
                      <Badge className={statusColor(lic.status)}>{t(`status.${lic.status}` as TranslationKeys)}</Badge>
                    </DataTableCell>
                    <DataTableCell className="text-muted-foreground text-xs text-right font-mono">
                      {formatDate(lic.created_at)}
                    </DataTableCell>
                  </DataTableRow>
                ))}
              </DataTableBody>
            </DataTable>
          </CardContent>
        </Card>
      )}
    </div>
  )
}
