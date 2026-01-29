import { createFileRoute } from '@tanstack/react-router'
import { Header } from '@/components/layout/header'
import { Main } from '@/components/layout/main'
import { ProfileDropdown } from '@/components/profile-dropdown'
import { ThemeSwitch } from '@/components/theme-switch'
import { ChatContainer } from '@/features/chat'

export const Route = createFileRoute('/_authenticated/chats/')({
  component: ChatPage,
})

function ChatPage() {
  return (
    <>
      <Header fixed>
        <div className='ms-auto flex items-center space-x-4'>
          <ThemeSwitch />
          <ProfileDropdown />
        </div>
      </Header>
      <Main fixed className='flex h-full min-h-0 flex-1 flex-col p-0'>
        <ChatContainer />
      </Main>
    </>
  )
}
