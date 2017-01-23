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
#include <iostream>

#include "../../../include/scanner/constructs/Results.h"
#include "../../../include/scanner/constructs/TypedResults.h"



int
main ()
{
    scanners::Results<int, scanners::ResultBlock<int> > res;
    int a = 34;
    res.add (&a);
    int b = 6;
    res.add (&b);
    int c = 9;
    res.add (&c);
    int d = 3;
    res.add (&d);

    scanners::Results<int, scanners::ResultBlock<int>>::iterator iter = res.begin ();

    scanners::TypedResults<int, float> riter (&iter);

    scanners::TypedResults<float, double> titer (&riter);

///*
    for (; titer != titer.end (); titer++)
    {
        std::cout << "Value " << (*titer).get() << std::endl;
    }

}

