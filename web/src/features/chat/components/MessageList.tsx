import { useEffect, useRef } from 'react'
import { ScrollArea } from '@/components/ui/scroll-area'
import type { ChatMessage } from '@/types'
import { cn } from '@/lib/utils'
import { MessageItem } from './MessageItem'
import { ThinkingIndicator } from './ThinkingIndicator'

interface MessageListProps {
  messages: ChatMessage[]
  isThinking: boolean
  streamingText: string
  className?: string
}

export function MessageList({
  messages,
  isThinking,
  streamingText,
  className,
}: MessageListProps) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const bottomRef = useRef<HTMLDivElement>(null)
  const stickToBottomRef = useRef(true)
  const lastMessageCountRef = useRef(messages.length)
  const lastMessageRoleRef = useRef<string | null>(null)

  // Auto scroll to bottom when new messages arrive
  useEffect(() => {
    // 查找 ScrollArea 的 viewport 元素
    const container = scrollRef.current?.querySelector('[data-slot="scroll-area-viewport"]') as HTMLElement
    if (!container) return

    const messageCountChanged = messages.length !== lastMessageCountRef.current
    const latestMessage = messages[messages.length - 1]
    const currentMessageRole = latestMessage?.role || null
    
    // 检测是否是新的用户消息：消息数量增加 + 最新消息是用户消息 + 之前最后一条不是用户消息
    const isNewUserMessage = 
      messageCountChanged && 
      latestMessage?.role === 'user' && 
      lastMessageRoleRef.current !== 'user'
    
    // 更新引用
    lastMessageCountRef.current = messages.length
    lastMessageRoleRef.current = currentMessageRole

    // 计算距离底部的距离
    const distanceToBottom =
      container.scrollHeight - container.scrollTop - container.clientHeight
    
    // 如果用户手动滚动到上面（距离底部 > 200px），且不是新用户消息，则保持位置
    if (distanceToBottom > 200 && !isNewUserMessage && !isThinking && !streamingText) {
      stickToBottomRef.current = false
      return
    }

    // 以下情况强制滚动到底部：
    // 1. 用户发送了新消息（必须滚动）
    // 2. AI正在思考或流式输出
    // 3. 之前已经接近底部（用户没有手动滚动）
    const shouldScroll = isNewUserMessage || isThinking || streamingText || stickToBottomRef.current
    
    if (shouldScroll) {
      stickToBottomRef.current = true
      const behavior = streamingText || isThinking || isNewUserMessage ? 'auto' : 'smooth'
      
      // 使用双重 requestAnimationFrame 确保 DOM 更新完成后再滚动
      requestAnimationFrame(() => {
        requestAnimationFrame(() => {
          // 优先使用 scrollTop 直接设置，更可靠
          if (container) {
            container.scrollTop = container.scrollHeight
          }
          // 备用方案：使用 scrollIntoView
          bottomRef.current?.scrollIntoView({ behavior, block: 'end' })
        })
      })
    }
  }, [messages.length, streamingText, isThinking])

  // Show streaming message as a temporary message
  const displayMessages = [...messages]
  if (streamingText) {
    displayMessages.push({
      id: 'streaming',
      role: 'assistant',
      content: streamingText,
      timestamp: new Date(),
      status: 'streaming',
    })
  }

  if (displayMessages.length === 0 && !isThinking) {
    return (
      <div className='flex h-full items-center justify-center p-4'>
        <div className='text-center'>
          <p className='text-lg font-medium text-muted-foreground'>
            Start a conversation
          </p>
          <p className='text-sm text-muted-foreground'>
            Select an agent and send a message to begin
          </p>
        </div>
      </div>
    )
  }

  return (
    <ScrollArea ref={scrollRef} className='h-full p-4'>
      <div className='mx-auto max-w-3xl space-y-4'>
        {displayMessages.map((message) => (
          <MessageItem
            key={message.id}
            message={message}
            isStreaming={message.status === 'streaming'}
          />
        ))}
        {isThinking && !streamingText && <ThinkingIndicator />}
        <div ref={bottomRef} />
      </div>
    </ScrollArea>
  )
}
