include Makefile.inc


CPPFLAGS=-g -std=c++11 -I/usr/local/include/thrift  -I /usr/local/include/zookeeper -DHAVE_CONFIG_H
RM=rm -f
PWD=`pwd`


OBJS=$(subst .cpp,.o,$(wildcard *.cpp))


all: test

clean: 
	$(RM) $(OBJS)

	

	
test:	$(OBJS)
	gcc -o test test.c $(OBJS) $(SUBLIB) $(LDFLAGS)  -lstdc++
	$(RM) $(OBJS)