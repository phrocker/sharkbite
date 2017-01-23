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

using namespace std;

#include "ThreadPool.h"
#include "Executor.h"


class TestExecutor : public Executor
{
public:
 ReturnType *run()
 {
  cout << "Hello world" << endl;
  return new ReturnType(SUCCESS,NULL);
 }
};
int main()
{
  ThreadPool poo;
  
  boost::shared_future<ReturnType*> ret = poo.submit(new TestExecutor());
  
   std::vector<boost::shared_future<ReturnType*> > pending_data; // vector of futures

   pending_data.push_back(ret);

 //   boost::wait_for_all(pending_data.begin(), pending_data.end());
 sleep(1);
 
  poo.close();
  
  
 
}
