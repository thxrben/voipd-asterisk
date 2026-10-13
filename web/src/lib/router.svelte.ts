// Minimal history-API router: the server serves index.html for every
// non-API path, so plain paths work on reload.

class Router {
  path = $state(window.location.pathname)
  search = $state(window.location.search)

  constructor() {
    window.addEventListener('popstate', () => this.sync())
  }

  private sync(): void {
    this.path = window.location.pathname
    this.search = window.location.search
  }

  get query(): URLSearchParams {
    return new URLSearchParams(this.search)
  }

  go(to: string, replace = false): void {
    if (to === window.location.pathname + window.location.search) return
    if (replace) window.history.replaceState(null, '', to)
    else window.history.pushState(null, '', to)
    this.sync()
    if (!replace) window.scrollTo(0, 0)
  }
}

export const router = new Router()

/** Click handler for <a href> that navigates without a page load. */
export function link(e: MouseEvent): void {
  const a = e.currentTarget as HTMLAnchorElement
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
  if (a.origin !== window.location.origin || a.target) return
  e.preventDefault()
  router.go(a.pathname + a.search)
}
