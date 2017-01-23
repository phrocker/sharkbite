include Makefile.inc


CPPFLAGS=-g -std=c++11 -I/usr/local/include/thrift  -I /usr/local/include/zookeeper -DHAVE_CONFIG_H
RM=rm -f
PWD=`pwd`


OBJS=$(subst .cpp,.o,$(wildcard *.cpp))


all: ClientExample

clean: 
	$(RM) $(OBJS)

	
ClientExample:	$(OBJS)
	g++ -o ClientExample $(OBJS) $(SUBLIB) $(LDFLAGS) $(CPPFLAGS)
	$(RM) $(OBJS)