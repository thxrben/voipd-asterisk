// Small notifications in the corner ("Saved", warnings).

interface Toast {
  id: number
  text: string
  tone: 'good' | 'warn' | 'bad'
}

class Toasts {
  items = $state<Toast[]>([])
  private next = 1

  show(text: string, tone: Toast['tone'] = 'good', ms = tone === 'good' ? 3000 : 8000): void {
    const id = this.next++
    this.items = [...this.items, { id, text, tone }]
    setTimeout(() => this.dismiss(id), ms)
  }

  dismiss(id: number): void {
    this.items = this.items.filter((t) => t.id !== id)
  }
}

export const toast = new Toasts()
