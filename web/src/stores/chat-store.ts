import { create } from 'zustand'
import type { ChatMessage, UploadedFile } from '@/types'

export interface PendingAttachment {
  id: string
  file: File
  status: 'pending' | 'uploading' | 'uploaded' | 'error'
  uploadedFile?: UploadedFile
  error?: string
}

interface ChatState {
  // State
  taskId: string | null
  agentId: string
  messages: ChatMessage[]
  isThinking: boolean
  streamingText: string
  isConnected: boolean
  attachments: PendingAttachment[]

  // Actions
  setTaskId: (taskId: string | null) => void
  setAgent: (agentId: string) => void
  addMessage: (message: Omit<ChatMessage, 'id' | 'timestamp'>) => void
  updateMessage: (id: string, updates: Partial<ChatMessage>) => void
  setThinking: (thinking: boolean) => void
  setStreamingText: (text: string) => void
  appendStreamingText: (text: string) => void
  clearStreamingText: () => void
  setConnected: (connected: boolean) => void
  clearChat: () => void
  initFromTask: (taskId: string, messages: ChatMessage[]) => void
  // Attachment actions
  addAttachment: (file: File) => string
  updateAttachment: (id: string, updates: Partial<PendingAttachment>) => void
  removeAttachment: (id: string) => void
  clearAttachments: () => void
  getUploadedFileIds: () => string[]
}

function generateId(): string {
  return `msg_${Date.now()}_${Math.random().toString(36).substring(2, 9)}`
}

function generateAttachmentId(): string {
  return `att_${Date.now()}_${Math.random().toString(36).substring(2, 9)}`
}

export const useChatStore = create<ChatState>((set, get) => ({
  // Initial state
  taskId: null,
  agentId: '',
  messages: [],
  isThinking: false,
  streamingText: '',
  isConnected: false,
  attachments: [],

  // Actions
  setTaskId: (taskId) => set({ taskId }),

  setAgent: (agentId) => set({ agentId }),

  addMessage: (message) =>
    set((state) => ({
      messages: [
        ...state.messages,
        {
          ...message,
          id: generateId(),
          timestamp: new Date(),
        },
      ],
    })),

  updateMessage: (id, updates) =>
    set((state) => ({
      messages: state.messages.map((msg) =>
        msg.id === id ? { ...msg, ...updates } : msg
      ),
    })),

  setThinking: (thinking) => set({ isThinking: thinking }),

  setStreamingText: (text) => set({ streamingText: text }),

  appendStreamingText: (text) =>
    set((state) => ({
      streamingText: state.streamingText + text,
    })),

  clearStreamingText: () => set({ streamingText: '' }),

  setConnected: (connected) => set({ isConnected: connected }),

  clearChat: () =>
    set({
      taskId: null,
      messages: [],
      isThinking: false,
      streamingText: '',
      isConnected: false,
      attachments: [],
    }),

  initFromTask: (taskId, messages) =>
    set({
      taskId,
      messages,
      isThinking: false,
      streamingText: '',
    }),

  // Attachment actions
  addAttachment: (file) => {
    const id = generateAttachmentId()
    set((state) => ({
      attachments: [
        ...state.attachments,
        { id, file, status: 'pending' },
      ],
    }))
    return id
  },

  updateAttachment: (id, updates) =>
    set((state) => ({
      attachments: state.attachments.map((att) =>
        att.id === id ? { ...att, ...updates } : att
      ),
    })),

  removeAttachment: (id) =>
    set((state) => ({
      attachments: state.attachments.filter((att) => att.id !== id),
    })),

  clearAttachments: () => set({ attachments: [] }),

  getUploadedFileIds: () => {
    const state = get()
    return state.attachments
      .filter((att) => att.status === 'uploaded' && att.uploadedFile)
      .map((att) => att.uploadedFile!.id)
  },
}))
