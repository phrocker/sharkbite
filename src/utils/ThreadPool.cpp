/**
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#include "utils/ThreadPool.h"


#ifndef SH_UNLIKELY
#ifdef __GNUC__
#define SH_UNLIKELY(val) (__builtin_expect((val), 0))
#define SH_LIKELY(val) (__builtin_expect((val), 1))
#else
      #define SH_UNLIKELY(val) (val)
      #define SH_LIKELY(val) (val)
      #endif
#endif


namespace utils {

template<typename T>
void ThreadPool<T>::run_tasks(std::shared_ptr<WorkerThread> thread) {
  thread->is_running_ = true;
  while (running_.load()) {
    if (SH_UNLIKELY(thread_reduction_count_ > 0)) {
      if (--thread_reduction_count_ >= 0) {
        deceased_thread_queue_.enqueue(thread);
        thread->is_running_ = false;
        break;
      } else {
        thread_reduction_count_++;
      }
    }

    Worker<T> task;
    if (worker_queue_.dequeueWait(task)) {
      {
        std::unique_lock<std::mutex> lock(worker_queue_mutex_);
        if (!task_status_[task.getIdentifier()]) {
          continue;
        } else if (!worker_queue_.isRunning()) {
          worker_queue_.enqueue(std::move(task));
          continue;
        }
      }
      if (task.run()) {
        if (task.getNextExecutionTime() <= std::chrono::steady_clock::now()) {
          // it can be rescheduled again as soon as there is a worker available
          worker_queue_.enqueue(std::move(task));
          continue;
        }
        // Task will be put to the delayed queue as next exec time is in the future
        std::unique_lock<std::mutex> lock(worker_queue_mutex_);
        bool need_to_notify =
            delayed_worker_queue_.empty() ||
                task.getNextExecutionTime() < delayed_worker_queue_.top().getNextExecutionTime();

        delayed_worker_queue_.push(std::move(task));
        if (need_to_notify) {
          delayed_task_available_.notify_all();
        }
      }
    } else {
      // The threadpool is running, but the ConcurrentQueue is stopped -> shouldn't happen during normal conditions
      // Might happen during startup or shutdown for a very short time
      if (running_.load()) {
        std::this_thread::sleep_for(std::chrono::milliseconds(1));
      }
    }
  }
  current_workers_--;
}

template<typename T>
void ThreadPool<T>::manage_delayed_queue() {
  while (running_) {
    std::unique_lock<std::mutex> lock(worker_queue_mutex_);

    // Put the tasks ready to run in the worker queue
    while (!delayed_worker_queue_.empty() &&
        delayed_worker_queue_.top().getNextExecutionTime() <= std::chrono::steady_clock::now()) {
      // I'm very sorry for this - committee must has been seriously drunk when the interface of prio queue was submitted.
      Worker<T> task = std::move(const_cast<Worker<T>&>(delayed_worker_queue_.top()));
      delayed_worker_queue_.pop();
      worker_queue_.enqueue(std::move(task));
    }
    if (delayed_worker_queue_.empty()) {
      delayed_task_available_.wait(lock);
    } else {
      auto wait_time = std::chrono::duration_cast<std::chrono::milliseconds>(
          delayed_worker_queue_.top().getNextExecutionTime() - std::chrono::steady_clock::now());
      delayed_task_available_.wait_for(lock, std::max(wait_time, std::chrono::milliseconds(1)));
    }
  }
}

template<typename T>
bool ThreadPool<T>::execute(Worker<T> &&task, std::future<T> &future) {
  {
    std::unique_lock<std::mutex> lock(worker_queue_mutex_);
    task_status_[task.getIdentifier()] = true;
  }
  future = std::move(task.getPromise()->get_future());
  worker_queue_.enqueue(std::move(task));

  task_count_++;

  return true;
}

template<typename T>
void ThreadPool<T>::manageWorkers() {
  for (int i = 0; i < max_worker_threads_; i++) {
    std::stringstream thread_name;
    thread_name << name_ << " #" << i;
    auto worker_thread = std::make_shared<WorkerThread>(thread_name.str());
    worker_thread->thread_ = createThread(std::bind(&ThreadPool::run_tasks, this, worker_thread));
    thread_queue_.push_back(worker_thread);
    current_workers_++;
  }

  if (daemon_threads_) {
    for (auto &thread : thread_queue_) {
      thread->thread_.detach();
    }
  }

for (auto &thread : thread_queue_) {
    if (thread->thread_.joinable())
    thread->thread_.join();
}

}

template<typename T>
void ThreadPool<T>::start() {
  
  std::lock_guard<std::recursive_mutex> lock(manager_mutex_);
  if (!running_) {
    running_ = true;
    worker_queue_.start();
    manager_thread_ = std::thread(&ThreadPool::manageWorkers, this);

    std::lock_guard<std::mutex> quee_lock(worker_queue_mutex_);
    delayed_scheduler_thread_ = std::thread(&ThreadPool<T>::manage_delayed_queue, this);
  }
}

template<typename T>
void ThreadPool<T>::stopTasks(const TaskId &identifier) {
  std::unique_lock<std::mutex> lock(worker_queue_mutex_);
  task_status_[identifier] = false;
}

template<typename T>
void ThreadPool<T>::resume() {
  if (!worker_queue_.isRunning()) {
    worker_queue_.start();
  }
}

template<typename T>
void ThreadPool<T>::pause() {
  if (worker_queue_.isRunning()) {
    worker_queue_.stop();
  }
}

template<typename T>
void ThreadPool<T>::shutdown() {
  if (running_.load()) {
    std::lock_guard<std::recursive_mutex> lock(manager_mutex_);
    running_.store(false);

    drain();

    task_status_.clear();
    if (manager_thread_.joinable()) {
      manager_thread_.join();
    }

    delayed_task_available_.notify_all();
    if (delayed_scheduler_thread_.joinable()) {
      delayed_scheduler_thread_.join();
    }

    for (const auto &thread : thread_queue_) {
      if (thread->thread_.joinable())
        thread->thread_.join();
    }

    thread_queue_.clear();
    current_workers_ = 0;
    while (!delayed_worker_queue_.empty()) {
      delayed_worker_queue_.pop();
    }

    worker_queue_.clear();
  }
}

template class utils::ThreadPool<utils::TaskRescheduleInfo>;
template class utils::ThreadPool<int>;
template class utils::ThreadPool<bool>;

} /* namespace utils */
