<script lang="ts">
  // Voicemail: messages of the mailboxes the user may see (admins: all),
  // with player, download and delete; personal greetings.
  import Trash from '@lucide/svelte/icons/trash-2'
  import Download from '@lucide/svelte/icons/download'
  import Upload from '@lucide/svelte/icons/upload'
  import { api } from '../lib/api'
  import { dateTime, duration, phone } from '../lib/format'
  import { live } from '../lib/live.svelte'
  import { link } from '../lib/router.svelte'
  import { toast } from '../lib/toast.svelte'
  import type { Mailbox, VoicemailMessage } from '../lib/types'

  let boxes = $state<Mailbox[]>([])
  let messages = $state<VoicemailMessage[]>([])
  let error = $state('')
  let loaded = $state(false)

  async function load() {
    try {
      const res = await api<{ mailboxes: Mailbox[]; voicemails: VoicemailMessage[] }>('/voicemails')
      boxes = res.mailboxes
      messages = res.voicemails
      error = ''
    } catch (e) {
      error = (e as Error).message
    } finally {
      loaded = true
    }
  }
  $effect(() => {
    void load()
    let t: ReturnType<typeof setTimeout> | undefined
    const off = live.on('voicemail.', () => {
      clearTimeout(t)
      t = setTimeout(() => void load(), 400)
    })
    return () => {
      off()
      clearTimeout(t)
    }
  })

  const byBox = $derived(
    boxes.map((b) => ({ box: b, messages: messages.filter((m) => m.extension_id === b.extension_id) })),
  )

  async function heard(m: VoicemailMessage, value = true) {
    if (m.heard === value) return
    try {
      await api(`/voicemails/${m.id}`, { method: 'PUT', json: { heard: value } })
      m.heard = value
      const b = boxes.find((x) => x.extension_id === m.extension_id)
      if (b) {
        b.new += value ? -1 : 1
        b.old += value ? 1 : -1
      }
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  async function remove(m: VoicemailMessage) {
    if (!confirm(`Delete the message from ${caller(m)}?`)) return
    try {
      await api(`/voicemails/${m.id}`, { method: 'DELETE' })
      toast.show('Message deleted')
      await load()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }

  const caller = (m: VoicemailMessage) =>
    m.caller_name || (m.caller.startsWith('+') ? phone(m.caller) : 'Unknown caller')

  // --- greetings ---
  let uploading = $state<number | null>(null)
  let greetingError = $state<Record<number, string>>({})

  async function upload(b: Mailbox, file: File | undefined) {
    if (!file) return
    uploading = b.extension_id
    greetingError = { ...greetingError, [b.extension_id]: '' }
    try {
      await api(`/extensions/${b.extension_id}/greeting`, {
        method: 'PUT', body: file, headers: { 'Content-Type': 'audio/wav' },
      })
      toast.show(`Greeting of ${b.extension} saved`)
      await load()
    } catch (e) {
      const err = e as { fields?: Record<string, string>; message: string }
      greetingError = { ...greetingError, [b.extension_id]: err.fields?.['file'] ?? err.message }
    } finally {
      uploading = null
    }
  }

  async function removeGreeting(b: Mailbox) {
    if (!confirm(`Use the standard greeting for ${b.extension} again?`)) return
    try {
      await api(`/extensions/${b.extension_id}/greeting`, { method: 'DELETE' })
      await load()
    } catch (e) {
      toast.show((e as Error).message, 'bad')
    }
  }
</script>

<div class="page">
  <div class="head">
    <h1>Voicemail</h1>
    <a href="/codes" onclick={link} class="muted small">Listen on the phone: dial <b>*98</b></a>
  </div>

  {#if error}<div class="error-box">{error}</div>{/if}

  {#if loaded && boxes.length === 0}
    <div class="empty card">You have no phone yet. Ask an admin to link an extension to your account.</div>
  {/if}

  {#each byBox as { box, messages: msgs } (box.extension_id)}
    <section class="card" aria-label="Mailbox {box.extension}">
      <header>
        <h2>{box.extension} {box.name}</h2>
        {#if box.new}<span class="badge info">{box.new} new</span>{/if}
        <span class="muted small">{msgs.length} {msgs.length === 1 ? 'message' : 'messages'}</span>
      </header>
      {#if msgs.length === 0}
        <div class="empty">No messages.</div>
      {:else}
        <ul class="messages">
          {#each msgs as m (m.id)}
            <li class:unheard={!m.heard}>
              <div class="meta">
                <b>{caller(m)}</b>
                {#if m.caller_name && m.caller.startsWith('+')}<span class="mono muted">{phone(m.caller)}</span>{/if}
                <span class="muted">{dateTime(m.received_at)} · {duration(m.duration_ms / 1000)}</span>
                {#if !m.heard}<span class="badge info">new</span>{/if}
              </div>
              <div class="controls">
                <audio controls preload="none" src="/api/v1/voicemails/{m.id}/audio" onplay={() => heard(m)}
                  aria-label="Message from {caller(m)}"></audio>
                {#if m.heard}
                  <button class="small-btn" onclick={() => heard(m, false)}>Mark as new</button>
                {:else}
                  <button class="small-btn" onclick={() => heard(m)}>Mark as heard</button>
                {/if}
                <a class="button icon" href="/api/v1/voicemails/{m.id}/audio" download aria-label="Download"
                  title="Download"><Download size={15} /></a>
                <button class="icon" onclick={() => remove(m)} aria-label="Delete message from {caller(m)}"
                  title="Delete"><Trash size={15} /></button>
              </div>
            </li>
          {/each}
        </ul>
      {/if}
      <div class="body greeting">
        <span><b>Greeting:</b> {box.greeting ? 'personal' : 'standard ("Leider ist gerade niemand erreichbar …")'}</span>
        {#if box.greeting}
          <audio controls preload="none" src="/api/v1/extensions/{box.extension_id}/greeting/audio"
            aria-label="Greeting of {box.extension}"></audio>
          <button class="small-btn" onclick={() => removeGreeting(box)}>Use standard greeting</button>
        {/if}
        <label class="button upload">
          <Upload size={15} /> {uploading === box.extension_id ? 'Uploading…' : 'Upload greeting (WAV)'}
          <input type="file" accept="audio/wav,.wav" class="sr-only" disabled={uploading !== null}
            aria-label="Upload greeting for {box.extension}"
            onchange={(e) => { void upload(box, e.currentTarget.files?.[0]); e.currentTarget.value = '' }} />
        </label>
        {#if greetingError[box.extension_id]}<p class="error-box small">{greetingError[box.extension_id]}</p>{/if}
        <p class="muted small hint">Or record it on your phone: dial *98, then 0. Uploads: WAV, mono, 16 bit, 8 or 16 kHz, up to 2 minutes.</p>
      </div>
    </section>
  {/each}
</div>

<style>
  .page {
    display: grid;
    gap: 1rem;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 1rem;
    flex-wrap: wrap;
  }
  header {
    gap: 0.6rem;
  }
  .small {
    font-size: 0.82rem;
  }
  .messages {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .messages li {
    display: flex;
    flex-wrap: wrap;
    justify-content: space-between;
    align-items: center;
    gap: 0.5rem 1rem;
    padding: 0.7rem 1rem;
    border-bottom: 1px solid var(--border);
  }
  .messages li.unheard {
    background: color-mix(in srgb, var(--accent) 6%, transparent);
  }
  .meta {
    display: flex;
    flex-wrap: wrap;
    gap: 0.25rem 0.6rem;
    align-items: baseline;
  }
  .controls {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    flex-wrap: wrap;
  }
  audio {
    height: 2.2rem;
    max-width: 100%;
  }
  .icon {
    padding: 0.35rem;
  }
  .small-btn {
    font-size: 0.8rem;
    padding: 0.3rem 0.55rem;
  }
  .greeting {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.5rem 0.8rem;
  }
  .upload {
    cursor: pointer;
    display: inline-flex;
    align-items: center;
    gap: 0.35rem;
  }
  .hint {
    flex-basis: 100%;
    margin: 0;
  }
  .error-box.small {
    flex-basis: 100%;
    margin: 0;
  }
</style>
