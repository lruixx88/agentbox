import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import rehypeSanitize, { defaultSchema } from 'rehype-sanitize'
import { Copy, Check } from 'lucide-react'
import { useState } from 'react'
import { cn } from '@/lib/utils'
import 'highlight.js/styles/github-dark.css'

interface MarkdownContentProps {
  content: string
  className?: string
}

function CodeBlock({
  language,
  codeString,
  children,
  codeClassName,
}: {
  language: string
  codeString: string
  children: React.ReactNode
  codeClassName?: string
}) {
  const [copied, setCopied] = useState(false)

  const copyCode = async () => {
    await navigator.clipboard.writeText(codeString)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <div className='relative group my-4'>
      <div className='flex items-center justify-between bg-muted/50 px-4 py-2 rounded-t-lg border-b'>
        <span className='text-xs text-muted-foreground font-mono'>{language}</span>
        <button
          onClick={copyCode}
          className='flex items-center gap-1.5 px-2 py-1 text-xs text-muted-foreground hover:text-foreground transition-colors rounded hover:bg-muted'
        >
          {copied ? (
            <>
              <Check className='h-3 w-3' />
              <span>Copied</span>
            </>
          ) : (
            <>
              <Copy className='h-3 w-3' />
              <span>Copy</span>
            </>
          )}
        </button>
      </div>
      <pre className={cn('overflow-x-auto rounded-b-lg p-4 bg-muted/30', codeClassName)}>
        <code className={codeClassName}>{children}</code>
      </pre>
    </div>
  )
}

export function MarkdownContent({ content, className }: MarkdownContentProps) {
  const sanitizeSchema = {
    ...defaultSchema,
    attributes: {
      ...defaultSchema.attributes,
      code: [...(defaultSchema.attributes?.code || []), 'className'],
      pre: [...(defaultSchema.attributes?.pre || []), 'className'],
      span: [...(defaultSchema.attributes?.span || []), 'className'],
      div: [...(defaultSchema.attributes?.div || []), 'className'],
      table: [...(defaultSchema.attributes?.table || []), 'className'],
      thead: [...(defaultSchema.attributes?.thead || []), 'className'],
      tbody: [...(defaultSchema.attributes?.tbody || []), 'className'],
      tr: [...(defaultSchema.attributes?.tr || []), 'className'],
      th: [...(defaultSchema.attributes?.th || []), 'className'],
      td: [...(defaultSchema.attributes?.td || []), 'className'],
      a: [...(defaultSchema.attributes?.a || []), 'href', 'target', 'rel'],
      img: [...(defaultSchema.attributes?.img || []), 'src', 'alt', 'title'],
    },
  }

  return (
    <div
      className={cn(
        'markdown-content prose prose-sm dark:prose-invert max-w-none break-words',
        'prose-headings:mt-6 prose-headings:first:mt-0',
        'prose-p:mb-4 prose-p:first:mt-0 prose-p:last:mb-0',
        'prose-hr:hidden',
        className
      )}
    >
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[[rehypeSanitize, sanitizeSchema], rehypeHighlight]}
        skipHtml
        components={{
          pre({ children }: any) {
            return <>{children}</>
          },
          code({ inline, className: codeClassName, children }: any) {
            const match = /language-(\w+)/.exec(codeClassName || '')
            const language = match ? match[1] : ''
            const codeString = String(children).replace(/\n$/, '')

            if (!inline && match) {
              return (
                <CodeBlock language={language} codeString={codeString} codeClassName={codeClassName}>
                  {children}
                </CodeBlock>
              )
            }

            return (
              <code
                className={cn(
                  'relative rounded bg-muted px-[0.3rem] py-[0.2rem] font-mono text-sm',
                  codeClassName
                )}
              >
                {children}
              </code>
            )
          },
          p({ children }: any) {
            // 如果p标签是空的或只包含空白，不渲染
            const content = String(children).trim()
            if (!content || content === '') return null
            // 如果内容只是空白字符或换行，不渲染
            if (/^[\s\n\r]*$/.test(content)) return null
            return <p className='mb-4 last:mb-0 leading-relaxed'>{children}</p>
          },
          h1({ children }: any) {
            return <h1 className='text-2xl font-bold mt-6 mb-4 first:mt-0'>{children}</h1>
          },
          h2({ children }: any) {
            return <h2 className='text-xl font-semibold mt-5 mb-3 first:mt-0'>{children}</h2>
          },
          h3({ children }: any) {
            return <h3 className='text-lg font-semibold mt-4 mb-2 first:mt-0'>{children}</h3>
          },
          ul({ children }: any) {
            return <ul className='list-disc list-inside mb-4 space-y-1'>{children}</ul>
          },
          ol({ children }: any) {
            return <ol className='list-decimal list-inside mb-4 space-y-1'>{children}</ol>
          },
          li({ children }: any) {
            return <li className='ml-4'>{children}</li>
          },
          blockquote({ children }: any) {
            return (
              <blockquote className='border-l-4 border-primary/50 pl-4 my-4 italic text-muted-foreground'>
                {children}
              </blockquote>
            )
          },
          a({ href, children }: any) {
            return (
              <a
                href={href}
                target='_blank'
                rel='noopener noreferrer'
                className='text-primary hover:underline'
              >
                {children}
              </a>
            )
          },
          table({ children }: any) {
            return (
              <div className='overflow-x-auto my-4'>
                <table className='min-w-full border-collapse border border-border'>{children}</table>
              </div>
            )
          },
          thead({ children }: any) {
            return <thead className='bg-muted'>{children}</thead>
          },
          th({ children }: any) {
            return <th className='border border-border px-4 py-2 text-left font-semibold'>{children}</th>
          },
          td({ children }: any) {
            return <td className='border border-border px-4 py-2'>{children}</td>
          },
          hr() {
            // 隐藏多余的hr标签，只在明确需要时显示
            return null
          },
          strong({ children }: any) {
            return <strong className='font-semibold'>{children}</strong>
          },
          em({ children }: any) {
            return <em className='italic'>{children}</em>
          },
        }}
      >
        {content}
      </ReactMarkdown>
    </div>
  )
}
