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
#ifndef CONCURRENT_THREADPOOL_H
#define CONCURRENT_THREADPOOL_H

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

using namespace std;
#include "Executor.h"
#include "ReturnType.h"


enum Priority { LOW, NORMAL, HIGH };



typedef boost::packaged_task<ReturnType*> task_t;

/**
    * We've been given a thread.
    *
    * **/
ReturnType *run ( Priority pri, Executor *executor ) {
    cout << "let's run" << endl;
    if ( pri == HIGH || pri == NORMAL ) {
        executor->run();
    } else {
        // take another task, then run this one
        executor->run();
    }

    return NULL;

}

class ThreadPool
{


public:
    ThreadPool ( unsigned short normalPriorityThreads = 10, unsigned short highPriorityThreads = 0 )
        : shutdown ( false ), threads(new boost::thread_group ),
          adminThreads(  new boost::thread_group ), work(io_service), adminWork(admin_service) {



        /**
         * Create normal priority threads
         * */
        for ( int i=0; i< normalPriorityThreads; i++ ) {
            threads->create_thread ( boost::bind ( &boost::asio::io_service::run,
                                                   &io_service ) );
        }


        for ( int i = 0; i < ( highPriorityThreads > boost::thread::hardware_concurrency() ? highPriorityThreads : boost::thread::hardware_concurrency() ); ++i ) {
            adminThreads->create_thread ( boost::bind ( &boost::asio::io_service::run,
                                          &admin_service ) );
        }

    }

    boost::shared_future<ReturnType*> submit ( Executor *executor, Priority pri = NORMAL ) {

        cout << "submitting" << endl;
        boost::shared_ptr<task_t> task = boost::make_shared<task_t> ( boost::bind ( &run, pri, executor ) );

        boost::shared_future<ReturnType*> fut ( task->get_future() );
        switch(pri)
        {
        case LOW:
        case NORMAL:
            io_service.post ( boost::bind ( &task_t::operator(), task ) );

            break;
        case HIGH:
            admin_service.post ( boost::bind ( &task_t::operator(), task ) );
            break;
        }




        return fut;
    }

    void close() {
        shutdown = true;

        // Stop all thread activity before deleting the thread group
        io_service.stop();

        admin_service.stop();

        // Allow threads to clean up while shared task pointers are still in scope.
        threads.reset();
        adminThreads.reset();
    }

protected:


    bool shutdown;
    boost::asio::io_service io_service;
    boost::asio::io_service admin_service;
    boost::scoped_ptr<boost::thread_group> threads;
    boost::scoped_ptr<boost::thread_group> adminThreads;
    boost::asio::io_service::work work;
    boost::asio::io_service::work adminWork;
};


#endif //CONCURRENT_THREADPOOL_H
