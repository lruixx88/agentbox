import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '@/services/api'
import type { TaskEvent } from '@/types'
import { useChatStore } from '@/stores/chat-store'
import { useAgents } from '@/hooks/useAgents'
import { useCreateTask, useAppendTurn, useCancelTask } from '@/hooks/useTasks'

const noisyLineMatchers: Array<(line: string) => boolean> = [
  (line) => /^\d{4}-\d{2}-\d{2}t/.test(line),
  (line) => line.startsWith('error:'),
  (line) => line.startsWith('error '),
  (line) => line.startsWith('deprecated:'),
  (line) => line.startsWith('mcp startup:'),
  (line) => line.startsWith('openai codex'),
  (line) => line.startsWith('workdir:'),
  (line) => line.startsWith('exec '),
  (line) => line.startsWith('model:'),
  (line) => line.startsWith('provider:'),
  (line) => line.startsWith('approval:'),
  (line) => line.startsWith('sandbox:'),
  (line) => line.startsWith('session id:'),
  (line) => line.startsWith('user '),
  (line) => line.startsWith('user:'),
  (line) => line.includes('approval:'),
  (line) => line.includes('sandbox:'),
  (line) => line.includes('session id:'),
  (line) => line.includes('model:'),
  (line) => line.includes('provider:'),
  (line) => line.includes('failed to refresh available models'),
  (line) => line.includes('exceeded retry limit'),
  (line) => line.includes('unexpected status 401'),
  (line) => line.includes('too many requests'),
]

const cleanModelOutput = (input: string) => {
  if (!input) return input
  const lines = input.split('\n')
  const kept: string[] = []
  for (const rawLine of lines) {
    let line = rawLine.trim()
    if (!line) continue
    const lower = line.toLowerCase()
    if (noisyLineMatchers.some((match) => match(lower))) continue

    // Strip mixed CLI logs in a single line: keep content before " exec ".
    if (lower.includes(' exec ')) {
      line = line.slice(0, lower.indexOf(' exec ')).trim()
    }
    if (lower.includes('codex ')) {
      const codexIndex = lower.indexOf('codex ')
      line = line.slice(codexIndex + 'codex '.length).trim()
    }

    // Drop leftover CLI execution lines.
    const dropIfContains = [
      ' exec ',
      ' in /workspace',
      'succeeded in',
      'exited ',
      'exit code',
      'no such file or directory',
      'unexpected status',
      'plan update',
      'execution failed',
    ]
    if (dropIfContains.some((needle) => line.toLowerCase().includes(needle)))
      continue

    // Remove stray role prefixes and inline traces.
    // 更彻底地移除用户问题相关的内容
    if (line.toLowerCase().startsWith('user ')) continue
    if (line.toLowerCase().startsWith('user:')) continue
    if (line.toLowerCase().startsWith('user：')) continue
    if (line.toLowerCase().startsWith('assistant ')) {
      line = line.slice('assistant '.length).trim()
    }
    if (line.toLowerCase().startsWith('assistant:')) {
      line = line.slice('assistant:'.length).trim()
    }
    if (line.toLowerCase().startsWith('assistant：')) {
      line = line.slice('assistant：'.length).trim()
    }
    
    // 移除明显的用户问题行（仅在行首且整行都是问题时）
    // 注意：不要过度过滤，因为AI回复也可能以这些词开头
    // 只在非常明确的情况下（整行很短且以问号结尾）才过滤
    if (line.length < 50 && line.match(/^(你|您).*[？?]$/)) {
      // 可能是用户问题，跳过
      continue
    }
    
    line = line
      .replace(/plan update\s*→.*$/i, '')
      .replace(/\bexec\b.*$/i, '')
      .trim()
    if (line.toLowerCase().startsWith('exec ')) continue
    if (line.toLowerCase().includes('exec cat:')) continue
    if (line.toLowerCase().includes('no such file or directory')) continue
    if (!line) continue
    kept.push(line)
  }
  const cleaned = kept.length > 0 ? kept.join('\n') : ''
  if (!cleaned) return ''
  if (cleaned.includes('<') && cleaned.includes('>')) {
    return cleaned.replace(/<[^>]*>/g, '').trim()
  }
  return cleaned
}

export function useChat() {
  const {
    taskId,
    agentId,
    messages,
    isThinking,
    streamingText,
    attachments,
    setTaskId,
    setAgent,
    addMessage,
    setThinking,
    setStreamingText,
    appendStreamingText,
    clearStreamingText,
    setConnected,
    clearChat,
    addAttachment,
    updateAttachment,
    removeAttachment,
    clearAttachments,
    getUploadedFileIds,
  } = useChatStore()

  const { data: agents = [] } = useAgents()
  const createTask = useCreateTask()
  const appendTurn = useAppendTurn()
  const cancelTask = useCancelTask()
  const eventSourceRef = useRef<EventSource | null>(null)
  const [isUploading, setIsUploading] = useState(false)
  const outputFetchInFlightRef = useRef(false)
  const lastOutputKeyRef = useRef<string | null>(null)
  const lastAssistantHashRef = useRef<string | null>(null)

  const pushAssistantMessage = useCallback(
    (content: string, status: 'sent' | 'error' = 'sent') => {
      const cleaned = cleanModelOutput(content)
      if (!cleaned) return
      const hash = `${status}:${cleaned}`
      if (lastAssistantHashRef.current === hash) return
      const currentMessages = useChatStore.getState().messages
      const last = currentMessages[currentMessages.length - 1]
      if (
        last &&
        last.role === 'assistant' &&
        last.content.trim() === cleaned.trim()
      ) {
        lastAssistantHashRef.current = hash
        return
      }
      addMessage({
        role: 'assistant',
        content: cleaned,
        status,
      })
      lastAssistantHashRef.current = hash
    },
    [addMessage]
  )

  const finalizeFromOutput = useCallback(
    async (turnKey?: string) => {
      const currentTaskId = useChatStore.getState().taskId
      if (!currentTaskId) return
      if (outputFetchInFlightRef.current) return
      if (turnKey && lastOutputKeyRef.current === turnKey) return

      outputFetchInFlightRef.current = true
      try {
        const output = await api.getTaskOutput(currentTaskId)
        const text =
          typeof output === 'string'
            ? output
            : (output as { text?: string })?.text
        pushAssistantMessage(text || '')
        if (turnKey) {
          lastOutputKeyRef.current = turnKey
        }
      } finally {
        outputFetchInFlightRef.current = false
      }
    },
    [pushAssistantMessage]
  )

  // Handle SSE events
  const handleEvent = useCallback(
    (event: TaskEvent) => {
      switch (event.type) {
        case 'task.started':
        case 'task.turn_started':
          setThinking(true)
          break

        case 'agent.thinking':
          setThinking(true)
          break

        case 'agent.message': {
          const data = event.data as { text?: string; content?: string }
          const text = data?.text || data?.content || ''
          const cleaned = cleanModelOutput(text)
          if (cleaned) {
            const current = useChatStore.getState().streamingText
            if (current && cleaned.startsWith(current)) {
              setStreamingText(cleaned)
            } else {
              appendStreamingText(cleaned)
            }
          }
          break
        }

        case 'task.completed':
        case 'task.turn_completed': {
          const currentStreamingText = useChatStore.getState().streamingText
          if (currentStreamingText) {
            pushAssistantMessage(currentStreamingText)
            clearStreamingText()
          } else {
            const data = event.data as
              | { turn_count?: number; turnCount?: number; reason?: string }
              | undefined
            const turnCount = data?.turn_count ?? data?.turnCount
            const currentTaskId = useChatStore.getState().taskId
            const turnKey =
              currentTaskId && turnCount
                ? `${currentTaskId}:${turnCount}`
                : undefined
            void finalizeFromOutput(turnKey)
          }
          setThinking(false)
          if (event.type === 'task.completed') {
            const data = event.data as { reason?: string } | undefined
            if (data?.reason === 'idle timeout') {
              setTaskId(null)
            }
          }
          break
        }

        case 'task.failed':
        case 'task.cancelled': {
          const data = event.data as { error?: string; http_code?: number; status_code?: number }
          const isCancelled = event.type === 'task.cancelled'
          const errorMessage = data?.error || ''
          const httpCode = data?.http_code || data?.status_code
          const isRateLimit = httpCode === 429 || 
            errorMessage.toLowerCase().includes('429') ||
            errorMessage.toLowerCase().includes('rate limit') ||
            errorMessage.toLowerCase().includes('too many requests')
          
          const currentStreamingText = useChatStore.getState().streamingText
          if (currentStreamingText.trim()) {
            pushAssistantMessage(
              currentStreamingText,
              isCancelled ? 'sent' : 'error'
            )
            clearStreamingText()
          }
          if (isCancelled) {
            pushAssistantMessage(
              '**任务已中断**\n\n用户取消了本次任务。'
            )
          } else if (!currentStreamingText.trim()) {
            // 429错误显示红色文本
            const errorText = isRateLimit 
              ? `**请求过于频繁 (429)**\n\n${errorMessage || 'API 请求频率过高，请稍后再试。'}`
              : (errorMessage || 'Task failed')
            pushAssistantMessage(errorText, 'error')
          }
          setThinking(false)
          setTaskId(null)
          break
        }
      }
    },
    [
      appendStreamingText,
      clearStreamingText,
      finalizeFromOutput,
      pushAssistantMessage,
      setStreamingText,
      setThinking,
      setTaskId,
    ]
  )

  // Connect to SSE when taskId changes
  useEffect(() => {
    if (!taskId) {
      eventSourceRef.current?.close()
      setConnected(false)
      return
    }

    const es = api.streamTaskEvents(taskId)
    eventSourceRef.current = es

    es.onopen = () => {
      setConnected(true)
    }

    es.addEventListener('task.started', (e) => {
      try {
        const data = JSON.parse(e.data)
        handleEvent({ type: 'task.started', data } as TaskEvent)
      } catch {
        // ignore parse errors
      }
    })

    es.addEventListener('task.turn_started', (e) => {
      try {
        const data = JSON.parse(e.data)
        handleEvent({ type: 'task.turn_started', data } as TaskEvent)
      } catch {
        // ignore parse errors
      }
    })

    es.addEventListener('agent.thinking', () => {
      handleEvent({ type: 'agent.thinking' } as TaskEvent)
    })

    es.addEventListener('agent.message', (e) => {
      try {
        const data = JSON.parse(e.data)
        handleEvent({ type: 'agent.message', data } as TaskEvent)
      } catch {
        // ignore parse errors
      }
    })

    es.addEventListener('task.completed', (e) => {
      try {
        const data = JSON.parse(e.data)
        handleEvent({ type: 'task.completed', data } as TaskEvent)
      } catch {
        // ignore parse errors
      }
    })

    es.addEventListener('task.turn_completed', (e) => {
      try {
        const data = JSON.parse(e.data)
        handleEvent({ type: 'task.turn_completed', data } as TaskEvent)
      } catch {
        // ignore parse errors
      }
    })

    es.addEventListener('task.failed', (e) => {
      try {
        const data = JSON.parse(e.data)
        handleEvent({ type: 'task.failed', data } as TaskEvent)
      } catch {
        // ignore parse errors
      }
    })

    es.addEventListener('task.cancelled', (e) => {
      try {
        const data = JSON.parse(e.data)
        handleEvent({ type: 'task.cancelled', data } as TaskEvent)
      } catch {
        // ignore parse errors
      }
    })

    es.onerror = () => {
      setConnected(false)
      es.close()
    }

    return () => {
      es.close()
      setConnected(false)
    }
  }, [taskId, setConnected, handleEvent])

  // Upload a single file
  const uploadFile = useCallback(
    async (attachmentId: string, file: File) => {
      updateAttachment(attachmentId, { status: 'uploading' })
      try {
        const uploadedFile = await api.uploadFile(file)
        updateAttachment(attachmentId, {
          status: 'uploaded',
          uploadedFile,
        })
        return uploadedFile
      } catch (error) {
        updateAttachment(attachmentId, {
          status: 'error',
          error: error instanceof Error ? error.message : 'Upload failed',
        })
        throw error
      }
    },
    [updateAttachment]
  )

  // Add files and start uploading
  const addFiles = useCallback(
    async (files: FileList) => {
      setIsUploading(true)
      const uploadPromises: Promise<void>[] = []

      for (let i = 0; i < files.length; i++) {
        const file = files[i]
        const attachmentId = addAttachment(file)

        const promise = uploadFile(attachmentId, file)
          .then(() => {})
          .catch(() => {
            // Error is already handled in uploadFile
          })
        uploadPromises.push(promise)
      }

      await Promise.all(uploadPromises)
      setIsUploading(false)
    },
    [addAttachment, uploadFile]
  )

  // Send message with attachments
  const sendMessage = useCallback(
    async (prompt: string) => {
      if (!agentId) {
        throw new Error('Please select an agent first')
      }

      const attachmentIds = getUploadedFileIds()

      let messageContent = prompt
      if (attachmentIds.length > 0) {
        const currentAttachments = useChatStore.getState().attachments
        const fileNames = currentAttachments
          .filter((att) => att.status === 'uploaded')
          .map((att) => att.file.name)
        messageContent = `${prompt}

Attachments: ${fileNames.join(', ')}`
      }

      addMessage({
        role: 'user',
        content: messageContent,
        status: 'sent',
      })

      clearAttachments()
      setThinking(true)

      try {
        if (taskId) {
          await appendTurn.mutateAsync({ taskId, prompt })
        } else {
          const task = await createTask.mutateAsync({
            agent_id: agentId,
            prompt,
            attachments: attachmentIds.length > 0 ? attachmentIds : undefined,
          })
          setTaskId(task.id)
        }
      } catch (error) {
        setThinking(false)
        pushAssistantMessage(
          error instanceof Error ? error.message : 'Failed to send message',
          'error'
        )
      }
    },
    [
      agentId,
      taskId,
      addMessage,
      setThinking,
      setTaskId,
      createTask,
      appendTurn,
      getUploadedFileIds,
      clearAttachments,
      pushAssistantMessage,
    ]
  )

  // Start new chat
  const newChat = useCallback(() => {
    eventSourceRef.current?.close()
    setConnected(false)
    clearChat()
  }, [clearChat, setConnected])

  // Interrupt current task
  const interrupt = useCallback(() => {
    if (!taskId) return
    const currentTaskId = taskId
    eventSourceRef.current?.close()
    setConnected(false)
    cancelTask.mutate(currentTaskId)
    const currentStreamingText = useChatStore.getState().streamingText
    if (currentStreamingText.trim()) {
      pushAssistantMessage(currentStreamingText)
      clearStreamingText()
    }
    pushAssistantMessage('**Task stopped**\n\nThe user cancelled this task.')
    setTaskId(null)
    setThinking(false)
  }, [
    taskId,
    cancelTask,
    clearStreamingText,
    pushAssistantMessage,
    setThinking,
    setTaskId,
    setConnected,
  ])

  return {
    // State
    taskId,
    agentId,
    messages,
    isThinking,
    streamingText,
    attachments,
    agents,
    isUploading,

    // Actions
    setAgent,
    sendMessage,
    newChat,
    interrupt,
    clearChat,
    addFiles,
    removeAttachment,

    // Loading states
    isCreating: createTask.isPending,
    isAppending: appendTurn.isPending,
  }
}
