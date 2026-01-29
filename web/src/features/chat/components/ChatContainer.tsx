import { MessageSquarePlus } from 'lucide-react'
import { useDockerAvailable } from '@/hooks/useSystemHealth'
import { Button } from '@/components/ui/button'
import { useChat } from '../hooks/useChat'
import { AgentSelector } from './AgentSelector'
import { ChatInput } from './ChatInput'
import { MessageList } from './MessageList'

export function ChatContainer() {
  const dockerAvailable = useDockerAvailable()
  const {
    taskId,
    agentId,
    messages,
    isThinking,
    streamingText,
    attachments,
    agents,
    isUploading,
    setAgent,
    sendMessage,
    newChat,
    interrupt,
    addFiles,
    removeAttachment,
    isCreating,
    isAppending,
  } = useChat()

  const isLoading = isCreating || isAppending
  const canSend = !!agentId && dockerAvailable && !isLoading

  const handleSend = (message: string) => {
    if (!canSend) return
    sendMessage(message)
  }

  return (
    <div className='flex h-full flex-col overflow-hidden'>
      {/* Header - Fixed */}
      <div className='flex shrink-0 items-center justify-between border-b px-4 py-3'>
        <div className='flex items-center gap-4'>
          <AgentSelector
            agents={agents}
            selectedId={agentId}
            onSelect={setAgent}
            disabled={isLoading}
          />
          {taskId && (
            <span className='text-xs text-muted-foreground'>
              Task: {taskId}
            </span>
          )}
        </div>
        <Button
          variant='outline'
          size='sm'
          onClick={newChat}
          disabled={messages.length === 0 && !taskId}
        >
          <MessageSquarePlus className='mr-2 h-4 w-4' />
          New Chat
        </Button>
      </div>

      {/* Docker warning - Fixed */}
      {!dockerAvailable && (
        <div className='shrink-0 border-b bg-yellow-50 px-4 py-2 text-center text-sm text-yellow-800 dark:bg-yellow-900/20 dark:text-yellow-200'>
          Docker is not available. Please start Docker to run agents.
        </div>
      )}

      {/* Messages - Scrollable */}
      <div className='flex-1 min-h-0 overflow-hidden'>
        <MessageList
          messages={messages}
          isThinking={isThinking}
          streamingText={streamingText}
        />
      </div>

      {/* Input - Fixed */}
      <div className='shrink-0 border-t bg-background'>
        <ChatInput
          onSend={handleSend}
          onAddFiles={addFiles}
          onRemoveAttachment={removeAttachment}
          attachments={attachments}
          disabled={!canSend}
          loading={isLoading || isThinking}
          uploading={isUploading}
          onInterrupt={interrupt}
          canInterrupt={!!taskId && (isThinking || isLoading)}
          placeholder={
            !agentId ? 'Select an agent to start chatting' : 'Type a message...'
          }
        />
      </div>
    </div>
  )
}
