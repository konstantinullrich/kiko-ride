// A tiny History API router: the Go server answers every page path with
// index.html, and this picks the page.
export const route = $state({ path: location.pathname, query: new URLSearchParams(location.search) })

export function navigate(to) {
  history.pushState(null, '', to)
  sync()
  window.scrollTo(0, 0)
}

function sync() {
  route.path = location.pathname
  route.query = new URLSearchParams(location.search)
}

window.addEventListener('popstate', sync)

// link handles a click on an internal link without reloading the page.
export function link(event) {
  if (event.metaKey || event.ctrlKey || event.shiftKey || event.button !== 0) return
  event.preventDefault()
  navigate(event.currentTarget.getAttribute('href'))
}
