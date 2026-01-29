import { Bot, User, AlertCircle } from 'lucide-react'
import { cn } from '@/lib/utils'
import type { ChatMessage } from '@/types'
import { MarkdownContent } from './MarkdownContent'

interface MessageItemProps {
  message: ChatMessage
  isStreaming?: boolean
}

export function MessageItem({ message, isStreaming }: MessageItemProps) {
  const isUser = message.role === 'user'
  const isSystem = message.role === 'system'
  const isError = message.status === 'error'

  if (isSystem) {
    return (
      <div className='flex justify-center py-2'>
        <span className='rounded-full bg-muted px-3 py-1 text-xs text-muted-foreground'>
          {message.content}
        </span>
      </div>
    )
  }

  return (
    <div
      className={cn(
        'flex items-start gap-3',
        isUser ? 'flex-row-reverse' : 'flex-row'
      )}
    >
      {/* Avatar */}
      <div
        className={cn(
          'flex h-8 w-8 shrink-0 items-center justify-center rounded-full',
          isUser ? 'bg-primary' : 'bg-primary/10'
        )}
      >
        {isUser ? (
          <User className='h-4 w-4 text-primary-foreground' />
        ) : (
          <Bot className='h-4 w-4 text-primary' />
        )}
      </div>

      {/* Message content */}
      <div
        className={cn(
          'flex min-w-0 max-w-[80%] flex-col gap-1',
          isUser ? 'items-end' : 'items-start'
        )}
      >
        <div
          className={cn(
            'rounded-2xl px-4 py-3 break-words overflow-hidden',
            isUser
              ? 'rounded-tr-none bg-primary text-primary-foreground'
              : 'rounded-tl-none bg-muted',
            isError && 'border border-destructive bg-destructive/10',
            isStreaming && !isUser && 'relative overflow-hidden'
          )}
        >
          {isError && (
            <div className='mb-2 flex items-center gap-1 text-destructive font-medium'>
              <AlertCircle className='h-3 w-3' />
              <span className='text-xs'>Error</span>
            </div>
          )}
          {isStreaming && !isUser && (
            <div className='absolute top-0 left-0 w-full h-full bg-gradient-to-r from-transparent via-white/5 to-transparent animate-shimmer pointer-events-none' />
          )}
          {isUser ? (
            <div className='text-sm whitespace-pre-wrap'>{message.content}</div>
          ) : (
            <MarkdownContent
              content={message.content}
              className={cn(
                'text-sm',
                isUser && 'prose-invert',
                isError && '[&_*]:!text-destructive'
              )}
            />
          )}
        </div>
        <span className='text-xs text-muted-foreground'>
          {formatTime(message.timestamp)}
        </span>
      </div>
    </div>
  )
}

function formatTime(date: Date): string {
  return date.toLocaleTimeString([], {
    hour: '2-digit',
    minute: '2-digit',
  })
}
