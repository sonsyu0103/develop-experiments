import React, {useEffect, useState} from 'react'
import {Thread} from '../../types/api'

export default function ThreadsPage() {
  const [threads, setThreads] = useState<Thread[]>([])

  useEffect(() => {
    fetch('http://localhost:8080/threads')
      .then(res => res.json())
      .then((data: Thread[]) => setThreads(data))
      .catch(err => console.error(err))
  }, [])

  return (
    <main>
      <h1>Threads</h1>
      <ul>
        {threads.map(t => (
          <li key={t.id}>{t.title} ({t.commentsCount} comments)</li>
        ))}
      </ul>
    </main>
  )
}
