import { Outlet } from '@tanstack/react-router'
import { AlertTriangle } from 'lucide-react'
import { getCookie } from '@/lib/cookies'
import { cn } from '@/lib/utils'
import { LayoutProvider } from '@/context/layout-provider'
import { SearchProvider } from '@/context/search-provider'
import { SidebarInset, SidebarProvider } from '@/components/ui/sidebar'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { AppSidebar } from '@/components/layout/app-sidebar'
import { SkipToMain } from '@/components/skip-to-main'
import { useSystemHealth } from '@/hooks/useSystemHealth'

type AuthenticatedLayoutProps = {
  children?: React.ReactNode
}

export function AuthenticatedLayout({ children }: AuthenticatedLayoutProps) {
  const defaultOpen = getCookie('sidebar_state') !== 'false'
  const { data: health } = useSystemHealth()

  const dockerUnavailable = health && health.docker?.status !== 'healthy'

  return (
    <SearchProvider>
      <LayoutProvider>
        <SidebarProvider defaultOpen={defaultOpen}>
          <SkipToMain />
          <AppSidebar />
          <SidebarInset
            className={cn(
              '@container/content',
              'has-data-[layout=fixed]:h-svh',
              'has-data-[layout=fixed]:min-h-0 has-data-[layout=fixed]:overflow-hidden',
              'peer-data-[variant=inset]:has-data-[layout=fixed]:h-[calc(100svh-(var(--spacing)*4))]'
            )}
          >
            {dockerUnavailable && (
              <Alert variant='destructive' className='mx-4 mt-4 border-amber-500 bg-amber-50 text-amber-900 dark:bg-amber-950 dark:text-amber-200'>
                <AlertTriangle className='size-4 text-amber-600' />
                <AlertTitle>Docker 不可用</AlertTitle>
                <AlertDescription>
                  无法连接到 Docker，任务执行功能不可用。请确认 Docker Desktop 已启动。
                  {health?.docker?.error && (
                    <span className='ml-1 text-xs opacity-70'>({health.docker.error})</span>
                  )}
                </AlertDescription>
              </Alert>
            )}
            {children ?? <Outlet />}
          </SidebarInset>
        </SidebarProvider>
      </LayoutProvider>
    </SearchProvider>
  )
}
