/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
#include <boost/shared_ptr.hpp>
#include <boost/scoped_ptr.hpp>
#include <boost/date_time/posix_time/posix_time.hpp>
#include <boost/make_shared.hpp>
#include <boost/thread.hpp>
#include <boost/bind.hpp>
#include <boost/asio.hpp>
#include <boost/chrono.hpp>
#include <boost/move/move.hpp>
#include <iostream>

boost::mutex mutex;

int sleep_print(int seconds)
{
    mutex.lock();
    std::cout << "goint to sleep (" << seconds << ")" << std::endl;
    mutex.unlock();
    boost::this_thread::sleep(boost::posix_time::milliseconds(seconds));
    mutex.lock();
    std::cout << "wake up (" << seconds << ")" << std::endl;
    mutex.unlock();
    return 0;
}

typedef boost::packaged_task<int> task_t;

void push_job(int seconds, boost::asio::io_service& io_service, std::vector<boost::shared_future<int> >& pending_data)
{
    boost::shared_ptr<task_t> task = boost::make_shared<task_t>(boost::bind(&sleep_print, seconds));
    boost::shared_future<int> fut(task->get_future());
    pending_data.push_back(fut);
    io_service.post(boost::bind(&task_t::operator(), task));
}

int main()
{
    boost::asio::io_service io_service;
    boost::scoped_ptr<boost::thread_group> threads( new boost::thread_group );
    boost::asio::io_service::work work(io_service);

    for (int i = 0; i < boost::thread::hardware_concurrency() ; ++i)
    {
            threads->create_thread(boost::bind(&boost::asio::io_service::run,
                    &io_service));
    }

    std::vector<boost::shared_future<int> > pending_data; // vector of futures

    push_job(1000, io_service, pending_data);
    push_job(1001, io_service, pending_data);
    push_job(1002, io_service, pending_data);
    push_job(1003, io_service, pending_data);
    push_job(1004, io_service, pending_data);
    push_job(1005, io_service, pending_data);
    push_job(1006, io_service, pending_data);
    push_job(1007, io_service, pending_data);
    push_job(1008, io_service, pending_data);
    push_job(1009, io_service, pending_data);
    push_job(1010, io_service, pending_data);

    boost::wait_for_all(pending_data.begin(), pending_data.end());

    // Stop all thread activity before deleting the thread group
    io_service.stop();

    // Allow threads to clean up while shared task pointers are still in scope.
    threads.reset();

    system("PAUSE");
        
    return 0;
}
